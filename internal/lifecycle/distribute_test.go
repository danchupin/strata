package lifecycle

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/leader"
	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/meta/memory"
)

func TestLeaseNamePin(t *testing.T) {
	got := LeaseName("11111111-2222-3333-4444-555555555555")
	want := "lifecycle-leader-11111111-2222-3333-4444-555555555555"
	if got != want {
		t.Fatalf("LeaseName=%q want %q", got, want)
	}
}

// TestBucketReplicaIndexDeterministic pins the distribution gate's hash:
// same input always maps to same replica index regardless of ring size.
func TestBucketReplicaIndexDeterministic(t *testing.T) {
	id := "deadbeef-aaaa-bbbb-cccc-001122334455"
	for _, count := range []int{1, 3, 16, 64} {
		a := bucketReplicaIndex(id, count)
		b := bucketReplicaIndex(id, count)
		if a != b {
			t.Fatalf("count=%d not deterministic: %d vs %d", count, a, b)
		}
		if a < 0 || a >= count {
			t.Fatalf("count=%d index=%d out of [0,%d)", count, a, count)
		}
	}
}

// TestBucketReplicaIndexDistribution sanity-checks the hash spread: 90
// buckets across 3 replicas land within ±50% of even.
func TestBucketReplicaIndexDistribution(t *testing.T) {
	const buckets = 90
	const replicas = 3
	hist := make([]int, replicas)
	for range buckets {
		id := uuid.New().String()
		hist[bucketReplicaIndex(id, replicas)]++
	}
	expected := buckets / replicas
	tolerance := expected / 2
	for r, n := range hist {
		if n < expected-tolerance || n > expected+tolerance {
			t.Errorf("replica %d got %d buckets (expected %d ± %d)", r, n, expected, tolerance)
		}
	}
}

// TestRunOnceLegacyPathProcessesEveryBucket: when Locker/ReplicaInfo are
// nil, the worker behaves as Phase 1 — every bucket processed sequentially.
// Pinning this is what keeps admin/bench callers (which don't wire the
// distributed knobs) functional.
func TestRunOnceLegacyPathProcessesEveryBucket(t *testing.T) {
	ctx := context.Background()
	store := memory.New()
	be := newCountingBackend()

	for i := range 5 {
		seedExpiringBucket(t, ctx, store, be, fmt.Sprintf("bk-%d", i))
	}

	w := &Worker{
		Meta:    store,
		Data:    be,
		Region:  "default",
		AgeUnit: time.Hour,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	for i := range 5 {
		assertBucketEmpty(t, ctx, store, fmt.Sprintf("bk-%d", i))
	}
}

// TestThreeReplicaDistribution drives the AC: 9 buckets, 3 replicas at
// STRATA_GC_SHARDS=3, each bucket processed exactly once per cycle, work
// distributed roughly 3-3-3.
//
// Each replica is a distinct *Worker bound to (a) a shared meta.Store +
// data.Backend (one cluster), (b) a shared in-memory locker (mirrors the
// production shape: distinct gateway processes racing for the same
// Cassandra/TiKV-backed lease keyspace), (c) a hard-coded ReplicaInfo
// returning the replica's index. The hard-coded id mimics the
// `min(GCFanOut.HeldShards())` derivation tested in gc/fanout_test.go but
// keeps this test independent of the FanOut leader-election lottery.
//
// Per-replica work attribution rides on a counting wrapper around the
// shared meta.Store: each replica's wrapper increments its own counter
// when DeleteObject fires, while delegating the actual mutation to the
// shared store. Sum across replicas must equal `buckets` (each bucket
// expired exactly once).
func TestThreeReplicaDistribution(t *testing.T) {
	ctx := context.Background()
	store := memory.New()
	be := newCountingBackend()
	locker := memory.NewLocker()

	const buckets = 9
	const replicas = 3
	for i := range buckets {
		seedExpiringBucket(t, ctx, store, be, fmt.Sprintf("bkt-%02d", i))
	}

	bs, err := store.ListBuckets(ctx, "")
	if err != nil {
		t.Fatalf("ListBuckets: %v", err)
	}
	expectedPerReplica := make([]int, replicas)
	for _, b := range bs {
		expectedPerReplica[bucketReplicaIndex(b.ID.String(), replicas)]++
	}

	type replicaState struct {
		w    *Worker
		meta *countingMeta
	}
	reps := make([]*replicaState, replicas)
	for i := range replicas {
		cm := &countingMeta{Store: store}
		w := &Worker{
			Meta:        cm,
			Data:        be,
			Region:      "default",
			AgeUnit:     time.Hour,
			Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
			Locker:      locker,
			ReplicaInfo: fixedReplicaInfo(replicas, i),
			LeaderTTL:   500 * time.Millisecond,
		}
		reps[i] = &replicaState{w: w, meta: cm}
	}

	// Run all three replicas concurrently — exercises the locker race.
	var wg sync.WaitGroup
	errs := make([]error, replicas)
	for i, r := range reps {
		wg.Go(func() {
			errs[i] = r.w.RunOnce(ctx)
		})
	}
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Fatalf("replica %d RunOnce: %v", i, e)
		}
	}

	totalDeletes := 0
	for i, r := range reps {
		got := r.meta.deleteObjects()
		if got != expectedPerReplica[i] {
			t.Errorf("replica %d processed %d buckets, hash distribution expected %d", i, got, expectedPerReplica[i])
		}
		totalDeletes += got
	}
	if totalDeletes != buckets {
		t.Fatalf("total DeleteObject count across replicas = %d, expected %d (each bucket exactly once)", totalDeletes, buckets)
	}

	// Each replica's load is roughly 3-3-3 — the hash distribution is
	// pinned by TestBucketReplicaIndexDistribution; this guard rejects
	// pathological hash skew.
	for i, n := range expectedPerReplica {
		if n < 1 || n > 5 {
			t.Errorf("replica %d expected to process %d buckets — distribution skewed", i, n)
		}
	}

	// Every bucket's seeded object expired.
	for _, b := range bs {
		_, err := store.GetObject(ctx, b.ID, "k", "")
		if err == nil {
			t.Errorf("bucket %s still has object — lifecycle did not run", b.Name)
		}
	}
}

// TestSkipCycleWhenNoReplicaIndex: ReplicaInfo returning (count>0, -1)
// short-circuits the cycle. Pins the AC: replicas holding zero gc shards
// skip lifecycle work that cycle.
func TestSkipCycleWhenNoReplicaIndex(t *testing.T) {
	ctx := context.Background()
	store := memory.New()
	be := newCountingBackend()
	seedExpiringBucket(t, ctx, store, be, "skipped")

	w := &Worker{
		Meta:    store,
		Data:    be,
		Region:  "default",
		AgeUnit: time.Hour,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Locker:  memory.NewLocker(),
		ReplicaInfo: func() (int, int) {
			return 3, -1 // no shards held — skip cycle
		},
	}
	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	bs, err := store.ListBuckets(ctx, "")
	if err != nil {
		t.Fatalf("ListBuckets: %v", err)
	}
	if len(bs) != 1 {
		t.Fatalf("listed %d buckets want 1", len(bs))
	}
	if _, err := store.GetObject(ctx, bs[0].ID, "k", ""); err != nil {
		t.Errorf("seed object expired even though cycle should have skipped: %v", err)
	}
}

// TestPerBucketLeaseSerialisesConcurrentReplicas verifies the per-bucket
// lease is the lock of last resort: with replicaCount=1 every replica
// passes the gate, but only one replica processes any given bucket per
// cycle.
func TestPerBucketLeaseSerialisesConcurrentReplicas(t *testing.T) {
	ctx := context.Background()
	store := memory.New()
	be := newCountingBackend()
	locker := memory.NewLocker()

	for i := range 4 {
		seedExpiringBucket(t, ctx, store, be, fmt.Sprintf("bk-%d", i))
	}

	const replicas = 3
	var wg sync.WaitGroup
	totals := make([]int, replicas)
	for i := range replicas {
		cm := &countingMeta{Store: store}
		w := &Worker{
			Meta:        cm,
			Data:        be,
			Region:      "default",
			AgeUnit:     time.Hour,
			Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
			Locker:      locker,
			ReplicaInfo: fixedReplicaInfo(1, 0), // every replica passes the gate
			LeaderTTL:   500 * time.Millisecond,
		}
		wg.Go(func() {
			if err := w.RunOnce(ctx); err != nil {
				t.Errorf("replica %d: %v", i, err)
				return
			}
			totals[i] = cm.deleteObjects()
		})
	}
	wg.Wait()

	sum := 0
	for _, n := range totals {
		sum += n
	}
	if sum != 4 {
		t.Fatalf("aggregate DeleteObject count=%d want 4 (each bucket exactly once)", sum)
	}
}

// fixedReplicaInfo returns a closure pinning the worker's gate output for
// the lifetime of one test. Mirrors the production shape derived from
// `workers.GCFanOut.HeldShards()` but holds the value steady for
// deterministic per-cycle assertions.
func fixedReplicaInfo(count, id int) func() (int, int) {
	return func() (int, int) { return count, id }
}

// seedExpiringBucket creates a bucket with one expired object and a
// 1-day expiration rule (AgeUnit=1h means the object is expired on the
// first cycle).
func seedExpiringBucket(t *testing.T, ctx context.Context, store *memory.Store, be data.Backend, name string) {
	t.Helper()
	b, err := store.CreateBucket(ctx, name, "owner", "STANDARD")
	if err != nil {
		t.Fatalf("CreateBucket(%s): %v", name, err)
	}
	payload := fmt.Appendf(nil, "payload-%s", name)
	manifest, err := be.PutChunks(ctx, bytes.NewReader(payload), "STANDARD")
	if err != nil {
		t.Fatalf("PutChunks: %v", err)
	}
	obj := &meta.Object{
		BucketID:     b.ID,
		Key:          "k",
		Size:         int64(len(payload)),
		ETag:         manifest.ETag,
		StorageClass: "STANDARD",
		Mtime:        time.Now().Add(-2 * time.Hour),
		Manifest:     manifest,
	}
	if err := store.PutObject(ctx, obj, false); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	rule := []byte(`<LifecycleConfiguration><Rule><ID>r</ID><Status>Enabled</Status>
		<Filter><Prefix></Prefix></Filter>
		<Expiration><Days>1</Days></Expiration>
	</Rule></LifecycleConfiguration>`)
	if err := store.SetBucketLifecycle(ctx, b.ID, rule); err != nil {
		t.Fatalf("SetBucketLifecycle: %v", err)
	}
}

func assertBucketEmpty(t *testing.T, ctx context.Context, store *memory.Store, name string) {
	t.Helper()
	bs, err := store.ListBuckets(ctx, "")
	if err != nil {
		t.Fatalf("ListBuckets: %v", err)
	}
	for _, b := range bs {
		if b.Name != name {
			continue
		}
		if _, err := store.GetObject(ctx, b.ID, "k", ""); err == nil {
			t.Fatalf("bucket %s still has object — lifecycle did not run", name)
		}
		return
	}
	t.Fatalf("bucket %s not found", name)
}

// countingBackend is a tiny in-memory data backend that records oid->bytes.
// Used as the shared cluster across replicas in the distribution tests.
type countingBackend struct {
	mu     sync.Mutex
	chunks map[string][]byte
	seq    int64
}

func newCountingBackend() *countingBackend {
	return &countingBackend{chunks: map[string][]byte{}}
}

func (b *countingBackend) PutChunks(ctx context.Context, r io.Reader, class string) (*data.Manifest, error) {
	if class == "" {
		class = "STANDARD"
	}
	body, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	h := md5.New()
	h.Write(body)
	b.mu.Lock()
	b.seq++
	id := b.seq
	oid := fmt.Sprintf("oid-%d", id)
	b.chunks[oid] = body
	b.mu.Unlock()
	return &data.Manifest{
		Class:  class,
		Size:   int64(len(body)),
		ETag:   hex.EncodeToString(h.Sum(nil)),
		Chunks: []data.ChunkRef{{Cluster: "default", Pool: "p", OID: oid, Size: int64(len(body))}},
	}, nil
}

func (b *countingBackend) GetChunks(ctx context.Context, m *data.Manifest, off, length int64) (io.ReadCloser, error) {
	var bufs [][]byte
	b.mu.Lock()
	for _, c := range m.Chunks {
		d, ok := b.chunks[c.OID]
		if !ok {
			b.mu.Unlock()
			return nil, fmt.Errorf("missing chunk %s", c.OID)
		}
		bufs = append(bufs, append([]byte(nil), d...))
	}
	b.mu.Unlock()
	full := bytes.Join(bufs, nil)
	if off > int64(len(full)) {
		off = int64(len(full))
	}
	if length <= 0 || off+length > int64(len(full)) {
		length = int64(len(full)) - off
	}
	return io.NopCloser(bytes.NewReader(full[off : off+length])), nil
}

func (b *countingBackend) Delete(ctx context.Context, m *data.Manifest) error {
	if m == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, c := range m.Chunks {
		delete(b.chunks, c.OID)
	}
	return nil
}

func (b *countingBackend) Close() error { return nil }

// countingMeta wraps *memory.Store and counts DeleteObject calls so each
// replica in the multi-replica tests has a private accounting view of the
// shared store. Mutations land on the shared inner store; per-replica
// ownership is observed via the counter.
type countingMeta struct {
	*memory.Store

	mu    sync.Mutex
	delCt int
}

func (c *countingMeta) DeleteObject(ctx context.Context, bucketID uuid.UUID, key, versionID string, versioned bool) (*meta.Object, error) {
	c.mu.Lock()
	c.delCt++
	c.mu.Unlock()
	return c.Store.DeleteObject(ctx, bucketID, key, versionID, versioned)
}

func (c *countingMeta) deleteObjects() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.delCt
}

// staticAssertions makes sure the locker and meta types satisfy the
// interfaces the tests rely on so a refactor in another package surfaces
// here at compile time rather than at runtime.
var (
	_ leader.Locker = (*memory.Locker)(nil)
	_ data.Backend  = (*countingBackend)(nil)
	_ meta.Store    = (*countingMeta)(nil)
)
