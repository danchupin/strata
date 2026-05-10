package quotareconcile

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/meta"
	metamem "github.com/danchupin/strata/internal/meta/memory"
)

func mustCreateBucket(t *testing.T, store meta.Store, name string) *meta.Bucket {
	t.Helper()
	b, err := store.CreateBucket(context.Background(), name, "owner", "STANDARD")
	if err != nil {
		t.Fatalf("create bucket %q: %v", name, err)
	}
	return b
}

func mustPutObject(t *testing.T, store meta.Store, bucketID [16]byte, key string, size int64) {
	t.Helper()
	if err := store.PutObject(context.Background(), &meta.Object{
		BucketID:     bucketID,
		Key:          key,
		StorageClass: "STANDARD",
		ETag:         `"x"`,
		Size:         size,
		Mtime:        time.Now().UTC(),
		Manifest: &data.Manifest{
			Class: "STANDARD", Size: size, ChunkSize: 4 * 1024 * 1024, ETag: `"x"`,
		},
	}, false); err != nil {
		t.Fatalf("put %q: %v", key, err)
	}
}

func TestRunOnceCorrectsBytesDrift(t *testing.T) {
	store := metamem.New()
	b := mustCreateBucket(t, store, "bkt")
	mustPutObject(t, store, b.ID, "a", 1000)
	mustPutObject(t, store, b.ID, "b", 2000)

	// Forge drift: bump stats by a chunk of bogus bytes.
	if _, err := store.BumpBucketStats(context.Background(), b.ID, 5_000_000, 0); err != nil {
		t.Fatalf("seed drift: %v", err)
	}
	pre, _ := store.GetBucketStats(context.Background(), b.ID)
	if pre.UsedBytes != 3000+5_000_000 {
		t.Fatalf("pre stats UsedBytes=%d, want %d", pre.UsedBytes, 3000+5_000_000)
	}

	w, err := New(Config{Meta: store, Logger: slog.Default(), MinDriftBytes: 1})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	stats, err := w.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if stats.BucketsScanned != 1 || stats.BucketsCorrected != 1 {
		t.Fatalf("stats=%+v", stats)
	}

	post, _ := store.GetBucketStats(context.Background(), b.ID)
	if post.UsedBytes != 3000 {
		t.Errorf("post UsedBytes=%d, want 3000", post.UsedBytes)
	}
	if post.UsedObjects != 2 {
		t.Errorf("post UsedObjects=%d, want 2", post.UsedObjects)
	}
}

func TestRunOnceCorrectsObjectsDrift(t *testing.T) {
	store := metamem.New()
	b := mustCreateBucket(t, store, "bkt")
	mustPutObject(t, store, b.ID, "k", 100)

	// Inflate object count without affecting bytes.
	if _, err := store.BumpBucketStats(context.Background(), b.ID, 0, 7); err != nil {
		t.Fatalf("seed: %v", err)
	}

	w, err := New(Config{Meta: store, Logger: slog.Default()})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if _, err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	post, _ := store.GetBucketStats(context.Background(), b.ID)
	if post.UsedObjects != 1 {
		t.Errorf("post UsedObjects=%d, want 1", post.UsedObjects)
	}
}

func TestRunOnceSkipsTinyDrift(t *testing.T) {
	store := metamem.New()
	b := mustCreateBucket(t, store, "bkt")
	mustPutObject(t, store, b.ID, "k", 1000)

	// Add 100 bytes of phantom drift — well below the 1 MiB default threshold.
	if _, err := store.BumpBucketStats(context.Background(), b.ID, 100, 0); err != nil {
		t.Fatalf("seed: %v", err)
	}

	w, err := New(Config{Meta: store, Logger: slog.Default()})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	stats, err := w.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if stats.BucketsCorrected != 0 {
		t.Fatalf("BucketsCorrected=%d, want 0 (drift below threshold)", stats.BucketsCorrected)
	}
	post, _ := store.GetBucketStats(context.Background(), b.ID)
	if post.UsedBytes != 1100 {
		t.Errorf("post UsedBytes=%d, want 1100 (untouched)", post.UsedBytes)
	}
}

func TestRunOnceWalksDeletedBucketWithoutBumping(t *testing.T) {
	store := metamem.New()
	b := mustCreateBucket(t, store, "bkt")
	mustPutObject(t, store, b.ID, "k", 100)

	w, err := New(Config{Meta: store, Logger: slog.Default(), MinDriftBytes: 1})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	stats, err := w.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if stats.BucketsScanned != 1 || stats.BucketsCorrected != 0 {
		t.Fatalf("stats=%+v (bucket already coherent)", stats)
	}
	if stats.ObjectsScanned != 1 {
		t.Errorf("ObjectsScanned=%d, want 1", stats.ObjectsScanned)
	}
}

func TestRunRespectsContextCancel(t *testing.T) {
	store := metamem.New()
	w, err := New(Config{Meta: store, Logger: slog.Default(), Interval: time.Hour})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v on cancel, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s after cancel")
	}
}

func TestDriftExceedsThreshold(t *testing.T) {
	cases := []struct {
		name        string
		drift, used int64
		objects     int64
		minBytes    int64
		minRatio    float64
		want        bool
	}{
		{"zero", 0, 100, 0, 1 << 20, 0.005, false},
		{"objects-trip", 0, 100, 1, 1 << 20, 0.005, true},
		{"under-min-bytes", 100, 100, 0, 1 << 20, 0.005, false},
		{"over-min-bytes", 1<<21, 100, 0, 1 << 20, 0.005, true},
		{"under-ratio", 100, 1 << 30, 0, 1, 0.005, false},
		{"over-ratio", 1 << 25, 1 << 30, 0, 1, 0.005, true},
		{"negative-over", -(1 << 25), 1 << 30, 0, 1, 0.005, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := driftExceedsThreshold(tc.drift, tc.objects, tc.used, tc.minBytes, tc.minRatio)
			if got != tc.want {
				t.Errorf("got=%v want=%v", got, tc.want)
			}
		})
	}
}

func TestNewRequiresMeta(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("want error for missing meta")
	}
}
