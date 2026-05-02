package bucketstats

import (
	"context"
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
