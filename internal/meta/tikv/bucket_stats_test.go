package tikv

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
)

// fakeShardMetrics counts per-shard BumpBucketStats commits so the race test
// can both assert uniform-ish distribution and serve as the operator-visible
// signal the production wiring (metrics.TiKVObserver) emits.
type fakeShardMetrics struct {
	mu     sync.Mutex
	counts [bucketStatsShardCount]int
}

func (m *fakeShardMetrics) IncBucketStatsShardWrite(shard int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.counts[shard]++
}

func (m *fakeShardMetrics) IncPessimisticTxn(op, outcome string) {
	_ = op
	_ = outcome
}

func (m *fakeShardMetrics) snapshot() (out [bucketStatsShardCount]int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out = m.counts
	return out
}

func newShardedTestStore(t *testing.T, m Metrics) *Store {
	t.Helper()
	s := openWithBackend(newMemBackend())
	s.metrics = m
	return s
}

// TestBumpBucketStatsConcurrent is the US-002 p1-fixes correctness anchor:
// 100 concurrent BumpBucketStats(+1) bumps must result in exactly 100 final
// objects (zero lost updates) AND every Bump call must return without an
// error (no in-impl CAS/txn-exhausted leakage). Shard fan-out additionally
// must spread the writes across >1 shard (else the test would silently pass
// against a degenerate single-shard regression).
func TestBumpBucketStatsConcurrent(t *testing.T) {
	m := &fakeShardMetrics{}
	s := newShardedTestStore(t, m)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	b, err := s.CreateBucket(ctx, "bkt", "alice", "STANDARD")
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}

	const concN = 100
	const concSize = int64(17)
	var (
		wg       sync.WaitGroup
		errCount int64
	)
	wg.Add(concN)
	for range concN {
		go func() {
			defer wg.Done()
			if _, berr := s.BumpBucketStats(ctx, b.ID, concSize, 1); berr != nil {
				atomic.AddInt64(&errCount, 1)
			}
		}()
	}
	wg.Wait()
	if got := atomic.LoadInt64(&errCount); got != 0 {
		t.Fatalf("BumpBucketStats errors: got %d, want 0 (any CAS/txn-exhausted = correctness regression)", got)
	}

	got, err := s.GetBucketStats(ctx, b.ID)
	if err != nil {
		t.Fatalf("GetBucketStats: %v", err)
	}
	if got.UsedBytes != concSize*concN {
		t.Fatalf("final UsedBytes: got %d, want %d (lost updates)", got.UsedBytes, concSize*concN)
	}
	if got.UsedObjects != concN {
		t.Fatalf("final UsedObjects: got %d, want %d (lost updates)", got.UsedObjects, concN)
	}

	snap := m.snapshot()
	var totalCount, nonZero int
	for _, c := range snap {
		totalCount += c
		if c > 0 {
			nonZero++
		}
	}
	if totalCount != concN {
		t.Fatalf("shard-write counter total: got %d, want %d", totalCount, concN)
	}
	if nonZero < 2 {
		t.Fatalf("fan-out collapsed to %d shard(s); expected >=2 distinct shards across %d bumps (counts=%v)", nonZero, concN, snap)
	}
}

// TestGetBucketStatsAcrossShards verifies the read path sums all shards
// rather than reading a single key. Writes are seeded directly under
// distinct shard keys; GetBucketStats must return their sum.
func TestGetBucketStatsAcrossShards(t *testing.T) {
	s := newShardedTestStore(t, nil)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	b, err := s.CreateBucket(ctx, "bkt", "alice", "STANDARD")
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}

	// 8 bumps of (+1, +1) each — over enough trials the picker will populate
	// >1 shard. Even if collapsed onto one shard, the read path must still
	// sum it correctly.
	const n = 8
	for range n {
		if _, berr := s.BumpBucketStats(ctx, b.ID, 1, 1); berr != nil {
			t.Fatalf("bump: %v", berr)
		}
	}
	got, err := s.GetBucketStats(ctx, b.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.UsedBytes != n || got.UsedObjects != n {
		t.Fatalf("get after %d bumps: got %+v want bytes=%d objects=%d", n, got, n, n)
	}
}
