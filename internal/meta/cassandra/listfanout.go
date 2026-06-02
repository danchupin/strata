package cassandra

import (
	"context"
	"os"
	"strconv"
	"strings"
	"sync"
)

// EnvListConcurrency caps how many shard partitions a single ListObjects /
// ListObjectVersions request queries concurrently (US-012).
const EnvListConcurrency = "STRATA_CASSANDRA_LIST_CONCURRENCY"

// DefaultListConcurrency is the fan-out worker-pool size when
// STRATA_CASSANDRA_LIST_CONCURRENCY is unset. Sized so a handful of concurrent
// listings against a high-shard-count bucket stay well inside the gocql
// connection pool rather than spawning one goroutine + one connection checkout
// per shard (the pre-US-012 unbounded fan-out exploded at shardCount ×
// concurrent-list-requests).
const DefaultListConcurrency = 16

// maxListConcurrency clamps an operator-supplied value. A request never needs
// more concurrent shard queries than this regardless of shard count.
const maxListConcurrency = 256

// listPageSize bounds the gocql page each shard cursor buffers. Decoupled from
// the request `limit` (up to 1000) so a high-shard-count listing's resident
// memory is shardCount × listPageSize rather than shardCount × (limit+1); the
// heap-merge auto-pages each cursor via advance() so a small page is purely a
// fetch-granularity choice, not a correctness or truncation one (truncation is
// decided by counting emitted rows, never by page exhaustion). Concurrent page
// fetches are additionally capped at boundedConcurrency × listPageSize by the
// fan-out worker pool.
const listPageSize = 256

// ListConcurrencyFromEnv reads STRATA_CASSANDRA_LIST_CONCURRENCY. Empty /
// invalid / <1 → DefaultListConcurrency; values above maxListConcurrency clamp
// down.
func ListConcurrencyFromEnv() int {
	v := strings.TrimSpace(os.Getenv(EnvListConcurrency))
	if v == "" {
		return DefaultListConcurrency
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return DefaultListConcurrency
	}
	if n > maxListConcurrency {
		return maxListConcurrency
	}
	return n
}

// runBoundedFanOut invokes fn for every shard in [0, shardCount) using at most
// `concurrency` concurrent goroutines, so a ListObjects fan-out checks out at
// most `concurrency` gocql connections at once regardless of how many shard
// partitions the bucket has (US-012). It returns the first error reported by
// any fn; fn callbacks append their own results under their own lock (the pool
// imposes no ordering). The shared work-burst is bounded — peak live worker
// goroutines ≤ concurrency — which is the whole point: the pre-US-012 loop
// spawned one goroutine per shard with no semaphore.
func runBoundedFanOut(ctx context.Context, shardCount, concurrency int, fn func(shard int) error) error {
	if shardCount <= 0 {
		return nil
	}
	if concurrency < 1 {
		concurrency = 1
	}
	if concurrency > shardCount {
		concurrency = shardCount
	}
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	errCh := make(chan error, shardCount)
	for shard := range shardCount {
		// Acquire BEFORE launching so the dispatch loop blocks once
		// `concurrency` workers are live — this is what caps the goroutine and
		// connection-pool footprint, not just the buffered channel.
		select {
		case <-ctx.Done():
			wg.Wait()
			return ctx.Err()
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(shard int) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := fn(shard); err != nil {
				errCh <- err
			}
		}(shard)
	}
	wg.Wait()
	close(errCh)
	return <-errCh
}
