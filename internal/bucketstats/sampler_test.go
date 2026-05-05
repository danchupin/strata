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
