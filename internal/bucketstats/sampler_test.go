package bucketstats

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/meta"
	metamem "github.com/danchupin/strata/internal/meta/memory"
)

type recordingSink struct {
	mu     sync.Mutex
	values map[string]int64
}

func newSink() *recordingSink { return &recordingSink{values: map[string]int64{}} }

func (s *recordingSink) SetBucketBytes(bucket, class string, bytes int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[bucket+"|"+class] = bytes
}

type recordingClassSink struct {
	mu      sync.Mutex
	bytes   map[string]int64 // bucket|class -> bytes
	objects map[string]int64 // bucket|class -> count
	resets  map[string]int   // bucket -> resetCount
}

func newClassSink() *recordingClassSink {
	return &recordingClassSink{
		bytes:   map[string]int64{},
		objects: map[string]int64{},
		resets:  map[string]int{},
	}
}

func (s *recordingClassSink) SetStorageClassBytes(bucket, class string, bytes int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bytes[bucket+"|"+class] = bytes
}

func (s *recordingClassSink) SetStorageClassObjects(bucket, class string, objects int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[bucket+"|"+class] = objects
}

func (s *recordingClassSink) ResetBucketClass(bucket string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resets[bucket]++
	for k := range s.bytes {
		if hasBucketPrefix(k, bucket) {
			delete(s.bytes, k)
		}
	}
	for k := range s.objects {
		if hasBucketPrefix(k, bucket) {
			delete(s.objects, k)
		}
	}
}

type recordingShardSink struct {
	mu      sync.Mutex
	bytes   map[string]int64 // bucket|shard -> bytes
	objects map[string]int64 // bucket|shard -> count
	resets  map[string]int   // bucket -> resetCount
}

func newShardSink() *recordingShardSink {
	return &recordingShardSink{
		bytes:   map[string]int64{},
		objects: map[string]int64{},
		resets:  map[string]int{},
	}
}

func (s *recordingShardSink) SetBucketShardBytes(bucket string, shard int, bytes int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bytes[fmt.Sprintf("%s|%d", bucket, shard)] = bytes
}

func (s *recordingShardSink) SetBucketShardObjects(bucket string, shard int, objects int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[fmt.Sprintf("%s|%d", bucket, shard)] = objects
}

func (s *recordingShardSink) ResetBucketShard(bucket string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resets[bucket]++
	for k := range s.bytes {
		if hasBucketPrefix(k, bucket) {
			delete(s.bytes, k)
		}
	}
	for k := range s.objects {
		if hasBucketPrefix(k, bucket) {
			delete(s.objects, k)
		}
	}
}

func hasBucketPrefix(key, bucket string) bool {
	return len(key) > len(bucket)+1 && key[:len(bucket)] == bucket && key[len(bucket)] == '|'
}

func TestSamplerAggregatesPerBucketAndClass(t *testing.T) {
	store := metamem.New()
	ctx := context.Background()
	b, err := store.CreateBucket(ctx, "bkt", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	now := time.Now().UTC()
	put := func(key, class string, size int64) {
		o := &meta.Object{
			BucketID:     b.ID,
			Key:          key,
			Size:         size,
			StorageClass: class,
			Mtime:        now,
			Manifest:     &data.Manifest{Size: size, Class: class},
		}
		if err := store.PutObject(ctx, o, false); err != nil {
			t.Fatalf("put: %v", err)
		}
	}
	put("a", "STANDARD", 100)
	put("b", "STANDARD", 200)
	put("c", "GLACIER", 1000)

	sink := newSink()
	s := &Sampler{Meta: store, Sink: sink, PageLimit: 10}
	if err := s.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if got := sink.values["bkt|STANDARD"]; got != 300 {
		t.Fatalf("STANDARD got %d, want 300", got)
	}
	if got := sink.values["bkt|GLACIER"]; got != 1000 {
		t.Fatalf("GLACIER got %d, want 1000", got)
	}
}

func TestSamplerNilSinkNoop(t *testing.T) {
	store := metamem.New()
	s := &Sampler{Meta: store}
	if err := s.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
}

// TestSamplerRunInitialPass verifies Sampler.Run emits a sample pass before
// the first ticker fires — the storage hero card relies on this so a fresh
// gateway boot does not have to wait an hour to populate the snapshot.
func TestSamplerRunInitialPass(t *testing.T) {
	store := metamem.New()
	ctx := context.Background()
	b, err := store.CreateBucket(ctx, "bk-initial", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	if err := store.PutObject(ctx, &meta.Object{
		BucketID:     b.ID,
		Key:          "k",
		Size:         5,
		StorageClass: "STANDARD",
		Mtime:        time.Now().UTC(),
		Manifest:     &data.Manifest{Size: 5, Class: "STANDARD"},
	}, false); err != nil {
		t.Fatalf("put object: %v", err)
	}

	snap := NewSnapshot(map[string]string{})
	s := &Sampler{
		Meta:     store,
		Snapshot: snap,
		// Long interval — test asserts initial pass before any tick.
		Interval: time.Hour,
	}
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = s.Run(runCtx)
		close(done)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(snap.Classes()) > 0 {
			cancel()
			<-done
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	t.Fatalf("snapshot still empty after initial pass deadline")
}

func TestSamplerEmitsPerShardForTopN(t *testing.T) {
	store := metamem.New()
	ctx := context.Background()
	now := time.Now().UTC()

	// Three buckets with different total bytes — top-2 should keep big+mid,
	// drop small.
	make := func(name string, sizes ...int64) {
		b, err := store.CreateBucket(ctx, name, "o", "STANDARD")
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		for i, sz := range sizes {
			o := &meta.Object{
				BucketID: b.ID, Key: fmt.Sprintf("%s-k%d", name, i),
				Size:         sz,
				StorageClass: "STANDARD",
				Mtime:        now,
				Manifest:     &data.Manifest{Size: sz, Class: "STANDARD"},
			}
			if err := store.PutObject(ctx, o, false); err != nil {
				t.Fatalf("put: %v", err)
			}
		}
	}
	make("big", 1000, 2000, 3000)
	make("mid", 50, 60, 70)
	make("small", 1)

	sink := newSink()
	shardSink := newShardSink()
	s := &Sampler{Meta: store, Sink: sink, ShardSink: shardSink, PageLimit: 100, TopN: 2}
	if err := s.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	// Per-class sink populated for ALL buckets (top-N only narrows the
	// per-shard pass).
	if sink.values["big|STANDARD"] != 6000 {
		t.Fatalf("big total: %d want 6000", sink.values["big|STANDARD"])
	}
	if sink.values["small|STANDARD"] != 1 {
		t.Fatalf("small total: %d want 1", sink.values["small|STANDARD"])
	}

	// big + mid present in shard sink; small absent.
	hasShardKey := func(bucket string) bool {
		shardSink.mu.Lock()
		defer shardSink.mu.Unlock()
		for k := range shardSink.bytes {
			if hasBucketPrefix(k, bucket) {
				return true
			}
		}
		return false
	}
	if !hasShardKey("big") {
		t.Fatalf("big should be in top-N shard sink")
	}
	if !hasShardKey("mid") {
		t.Fatalf("mid should be in top-N shard sink")
	}
	if hasShardKey("small") {
		t.Fatalf("small must NOT be in top-N shard sink (TopN=2)")
	}

	// Sum of per-shard bytes for big should equal 6000 (matches total).
	var bigSum int64
	shardSink.mu.Lock()
	for k, v := range shardSink.bytes {
		if hasBucketPrefix(k, "big") {
			bigSum += v
		}
	}
	shardSink.mu.Unlock()
	if bigSum != 6000 {
		t.Fatalf("big shard-sum: got %d want 6000", bigSum)
	}
}

func TestSamplerEmitsPerClassAndUpdatesSnapshot(t *testing.T) {
	store := metamem.New()
	ctx := context.Background()
	now := time.Now().UTC()

	mkBucket := func(name string, byClass map[string][]int64) {
		b, err := store.CreateBucket(ctx, name, "o", "STANDARD")
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		i := 0
		for class, sizes := range byClass {
			for _, sz := range sizes {
				o := &meta.Object{
					BucketID: b.ID, Key: fmt.Sprintf("%s-%s-%d", name, class, i),
					Size:         sz,
					StorageClass: class,
					Mtime:        now,
					Manifest:     &data.Manifest{Size: sz, Class: class},
				}
				if err := store.PutObject(ctx, o, false); err != nil {
					t.Fatalf("put: %v", err)
				}
				i++
			}
		}
	}
	mkBucket("alpha", map[string][]int64{
		"STANDARD": {100, 200},
		"GLACIER":  {1000},
	})
	mkBucket("beta", map[string][]int64{
		"STANDARD": {50},
	})

	classSink := newClassSink()
	snap := NewSnapshot(map[string]string{
		"STANDARD": "p1",
		"GLACIER":  "p2",
	})
	s := &Sampler{
		Meta:      store,
		ClassSink: classSink,
		Snapshot:  snap,
		PageLimit: 100,
		TopN:      10,
	}
	if err := s.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	// Per-(bucket, class) sink populated for both buckets.
	if got := classSink.bytes["alpha|STANDARD"]; got != 300 {
		t.Errorf("alpha|STANDARD bytes: got %d want 300", got)
	}
	if got := classSink.objects["alpha|STANDARD"]; got != 2 {
		t.Errorf("alpha|STANDARD objects: got %d want 2", got)
	}
	if got := classSink.bytes["alpha|GLACIER"]; got != 1000 {
		t.Errorf("alpha|GLACIER bytes: got %d want 1000", got)
	}
	if got := classSink.objects["alpha|GLACIER"]; got != 1 {
		t.Errorf("alpha|GLACIER objects: got %d want 1", got)
	}
	if got := classSink.bytes["beta|STANDARD"]; got != 50 {
		t.Errorf("beta|STANDARD bytes: got %d want 50", got)
	}

	// Snapshot carries cluster-wide totals.
	totals := snap.Classes()
	if totals["STANDARD"].Bytes != 350 || totals["STANDARD"].Objects != 3 {
		t.Errorf("snapshot STANDARD: %+v want bytes=350 objects=3", totals["STANDARD"])
	}
	if totals["GLACIER"].Bytes != 1000 || totals["GLACIER"].Objects != 1 {
		t.Errorf("snapshot GLACIER: %+v want bytes=1000 objects=1", totals["GLACIER"])
	}
	pools := snap.Pools()
	if pools["STANDARD"] != "p1" || pools["GLACIER"] != "p2" {
		t.Errorf("snapshot pools: %+v", pools)
	}
}

func TestSamplerClassSinkRespectsTopN(t *testing.T) {
	store := metamem.New()
	ctx := context.Background()
	now := time.Now().UTC()

	mk := func(name string, sz int64) {
		b, err := store.CreateBucket(ctx, name, "o", "STANDARD")
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		o := &meta.Object{
			BucketID: b.ID, Key: name + "-k",
			Size:         sz,
			StorageClass: "STANDARD",
			Mtime:        now,
			Manifest:     &data.Manifest{Size: sz, Class: "STANDARD"},
		}
		if err := store.PutObject(ctx, o, false); err != nil {
			t.Fatalf("put: %v", err)
		}
	}
	mk("big", 5000)
	mk("mid", 100)
	mk("tiny", 1)

	classSink := newClassSink()
	s := &Sampler{Meta: store, ClassSink: classSink, PageLimit: 100, TopN: 2}
	if err := s.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if _, ok := classSink.bytes["big|STANDARD"]; !ok {
		t.Errorf("big should be in top-N class sink")
	}
	if _, ok := classSink.bytes["mid|STANDARD"]; !ok {
		t.Errorf("mid should be in top-N class sink")
	}
	if _, ok := classSink.bytes["tiny|STANDARD"]; ok {
		t.Errorf("tiny must NOT be in top-N class sink (TopN=2)")
	}
}

func TestSamplerResetsShardsLeavingTopN(t *testing.T) {
	store := metamem.New()
	ctx := context.Background()
	now := time.Now().UTC()

	bigBucket, _ := store.CreateBucket(ctx, "alpha", "o", "STANDARD")
	smallBucket, _ := store.CreateBucket(ctx, "beta", "o", "STANDARD")
	put := func(b *meta.Bucket, key string, size int64) {
		o := &meta.Object{
			BucketID: b.ID, Key: key,
			Size: size, StorageClass: "STANDARD", Mtime: now,
			Manifest: &data.Manifest{Size: size, Class: "STANDARD"},
		}
		if err := store.PutObject(ctx, o, false); err != nil {
			t.Fatalf("put: %v", err)
		}
	}
	put(bigBucket, "k1", 1000)
	put(smallBucket, "k1", 10)

	sink := newSink()
	shardSink := newShardSink()
	s := &Sampler{Meta: store, Sink: sink, ShardSink: shardSink, PageLimit: 100, TopN: 1}
	if err := s.RunOnce(ctx); err != nil {
		t.Fatalf("first run: %v", err)
	}
	// alpha should be in top-N this pass.
	if got := shardSink.resets["alpha"]; got != 1 {
		t.Fatalf("alpha reset on first pass: got %d want 1", got)
	}
	if got := shardSink.resets["beta"]; got != 0 {
		t.Fatalf("beta reset on first pass: got %d want 0", got)
	}

	// Flip the totals — shrink alpha, grow beta. Now beta should be top-N.
	if _, err := store.DeleteObject(ctx, bigBucket.ID, "k1", "", false); err != nil {
		t.Fatalf("delete: %v", err)
	}
	put(smallBucket, "k2", 9999)

	if err := s.RunOnce(ctx); err != nil {
		t.Fatalf("second run: %v", err)
	}
	// alpha exited the top-N → ResetBucketShard called once for alpha
	// (the previous-set cleanup); beta now in top-N → reset+re-emit.
	if got := shardSink.resets["alpha"]; got < 2 {
		t.Fatalf("alpha must be reset on exit; resets=%d", got)
	}
	if got := shardSink.resets["beta"]; got < 1 {
		t.Fatalf("beta must be reset on entry; resets=%d", got)
	}
}
