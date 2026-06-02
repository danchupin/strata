//go:build integration

package cassandra_test

// Concurrency probe for ralph/p1-fixes US-001 — measures Cassandra
// BumpBucketStats behavior under 100 concurrent +1 bumps so the cycle's
// decision tree (a/b/c) is anchored on observed numbers, not assumption.
//
// The test deliberately runs OUTSIDE TestCassandraStoreContract because it
// owns a SessionConfig with a custom Metrics sink — the contract suite uses
// `cassandra.Open` without Metrics so its observer wiring stays minimal.
//
// Outcome captured in scripts/ralph/progress.txt as the contract for US-002+.
// Throwaway probe per US-001 AC "probe code OK as throwaway test if needed".

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danchupin/strata/internal/meta/cassandra"
)

// probeMetrics counts cassandra query observations grouped by (table, op) +
// fans LWT-conflict events into a separate counter. The LWT-conflict counter
// is unused for bucket_stats (the CAS loop in store.go:3447 does NOT call
// obs.RecordLWTConflict on applied=false) — kept for parity with the Metrics
// interface contract.
type probeMetrics struct {
	mu       sync.Mutex
	byTable  map[string]map[string]int
	failures int
}

func newProbeMetrics() *probeMetrics {
	return &probeMetrics{byTable: make(map[string]map[string]int)}
}

func (p *probeMetrics) ObserveQuery(table, op string, _ time.Duration, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.byTable[table]; !ok {
		p.byTable[table] = make(map[string]int)
	}
	p.byTable[table][op]++
	if err != nil {
		p.failures++
	}
}

func (p *probeMetrics) IncLWTConflict(_, _, _ string) {
	// no-op: bucket_stats CAS site does not emit LWT-conflict events
}

func (p *probeMetrics) snapshot() (insertCount, updateCount, selectCount, failures int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if ops, ok := p.byTable["bucket_stats"]; ok {
		insertCount = ops["INSERT"]
		updateCount = ops["UPDATE"]
		selectCount = ops["SELECT"]
	}
	failures = p.failures
	return
}

// TestCassandraBucketStatsConcurrencyProbe is the US-001 spike probe. It
// launches 100 concurrent BumpBucketStats(+1, +1) calls against a fresh
// bucket_stats row and reports:
//   - final UsedBytes / UsedObjects (== 100 if no lost updates)
//   - INSERT + UPDATE + SELECT query counts on the bucket_stats table
//   - error count for any "CAS exhausted retries" failures
//
// The numbers drive the US-001 → US-003 decision tree:
//
//	branch (a) — final=100, zero CAS-exhausted: fan-out TiKV only
//	branch (b) — final<100 OR CAS-exhausted: Cassandra also needs fan-out
//	branch (c) — different failure (deadlock, timeout): halt cycle
//
// Skipped under STRATA_SCYLLA_TEST=1 so it stays a single-engine probe.
func TestCassandraBucketStatsConcurrencyProbe(t *testing.T) {
	if os.Getenv("STRATA_SCYLLA_TEST") == "1" {
		t.Skip("STRATA_SCYLLA_TEST=1: probe runs against cassandra only")
	}

	ctx := context.Background()

	host := startCassandra(t)

	metrics := newProbeMetrics()
	store, err := cassandra.Open(cassandra.SessionConfig{
		Hosts:       []string{host},
		Keyspace:    "strata_probe",
		LocalDC:     "datacenter1",
		Replication: "{'class': 'SimpleStrategy', 'replication_factor': '1'}",
		Timeout:     60 * time.Second,
		Metrics:     metrics,
	}, cassandra.Options{DefaultShardCount: 64})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	bucket, err := store.CreateBucket(ctx, "probe", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}

	// CreateBucket does NOT touch bucket_stats (only BumpBucketStats writes
	// the row, verified store.go:3464 is the only INSERT into bucket_stats).
	// snapshot() filters to table="bucket_stats" below, so setup queries on
	// the buckets / bucket_names tables don't contaminate the count.

	const concurrency = 100
	var wg sync.WaitGroup
	var casExhausted atomic.Int64
	start := make(chan struct{})
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := store.BumpBucketStats(ctx, bucket.ID, 1, 1); err != nil {
				if strings.Contains(err.Error(), "CAS exhausted") {
					casExhausted.Add(1)
				} else {
					t.Errorf("bump: %v", err)
				}
			}
		}()
	}
	close(start)
	wg.Wait()

	final, err := store.GetBucketStats(ctx, bucket.ID)
	if err != nil {
		t.Fatalf("final get: %v", err)
	}

	insertCount, updateCount, selectCount, failures := metrics.snapshot()

	// CAS attempts on bucket_stats = INSERT + UPDATE. retries = attempts - 100
	// (one successful write per goroutine).
	attempts := insertCount + updateCount
	retries := attempts - concurrency
	if retries < 0 {
		retries = 0
	}

	report := fmt.Sprintf(
		`
--- BucketStats Concurrency Probe (US-001) ---
  concurrency        = %d
  final UsedBytes    = %d (expected %d if no lost updates)
  final UsedObjects  = %d (expected %d)
  bucket_stats INSERT count = %d
  bucket_stats UPDATE count = %d
  bucket_stats SELECT count = %d
  total CAS attempts        = %d
  retry count (= attempts - %d) = %d
  CAS-exhausted errors      = %d
  observer failure count    = %d
----------------------------------------------
`,
		concurrency,
		final.UsedBytes, concurrency,
		final.UsedObjects, concurrency,
		insertCount, updateCount, selectCount,
		attempts, concurrency, retries,
		casExhausted.Load(),
		failures,
	)
	t.Log(report)

	// Decision-tree assertions — the test passes if the spike captured
	// usable numbers. The branch (a/b/c) is documented in progress.txt by
	// the operator reading the t.Log report above.
	if final.UsedBytes != concurrency {
		t.Logf("BRANCH (b/c) DETECTED: final UsedBytes %d != %d — Cassandra LOST updates",
			final.UsedBytes, concurrency)
	}
	if casExhausted.Load() > 0 {
		t.Logf("BRANCH (b) DETECTED: %d CAS-exhausted errors observed at c=%d",
			casExhausted.Load(), concurrency)
	}
	if final.UsedBytes == concurrency && casExhausted.Load() == 0 {
		t.Logf("BRANCH (a) DETECTED: final=%d + zero CAS-exhausted — Cassandra absorbs c=%d with %d retries; "+
			"max attempts=32 headroom factor = %.1fx",
			concurrency, concurrency, retries, 32.0/float64(maxPerGoroutine(retries, concurrency)))
	}
}

func maxPerGoroutine(retries, concurrency int) int {
	// average retries per goroutine, floor 1 to avoid divide-by-zero in the
	// headroom factor log.
	avg := retries / concurrency
	if avg < 1 {
		avg = 1
	}
	return avg
}
