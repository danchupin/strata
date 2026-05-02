package workers

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/danchupin/strata/internal/data"
	datamem "github.com/danchupin/strata/internal/data/memory"
	"github.com/danchupin/strata/internal/manifestrewriter"
	"github.com/danchupin/strata/internal/meta"
	metamem "github.com/danchupin/strata/internal/meta/memory"
)

func TestManifestRewriterWorkerRegistered(t *testing.T) {
	w, ok := Lookup("manifest-rewriter")
	if !ok {
		t.Fatal("manifest-rewriter worker not registered (init() did not fire)")
	}
	if w.Name != "manifest-rewriter" {
		t.Fatalf("name=%q want manifest-rewriter", w.Name)
	}
}

func TestBuildManifestRewriterReadsEnv(t *testing.T) {
	t.Setenv("STRATA_MANIFEST_REWRITER_INTERVAL", "1h")
	t.Setenv("STRATA_MANIFEST_REWRITER_BATCH_LIMIT", "1000")
	t.Setenv("STRATA_MANIFEST_REWRITER_DRY_RUN", "true")

	r, err := buildManifestRewriter(Dependencies{Meta: metamem.New(), Data: datamem.New()})
	if err != nil {
		t.Fatalf("buildManifestRewriter: %v", err)
	}
	rr, ok := r.(*manifestRewriterRunner)
	if !ok {
		t.Fatalf("buildManifestRewriter returned %T, want *manifestRewriterRunner", r)
	}
	if rr.interval != time.Hour {
		t.Errorf("interval = %v, want 1h", rr.interval)
	}
	if !rr.dryRun {
		t.Errorf("dryRun = false, want true")
	}
	if rr.worker == nil {
		t.Errorf("worker not constructed")
	}
}

func TestBuildManifestRewriterDefaultsWhenEnvUnset(t *testing.T) {
	t.Setenv("STRATA_MANIFEST_REWRITER_INTERVAL", "")
	t.Setenv("STRATA_MANIFEST_REWRITER_BATCH_LIMIT", "")
	t.Setenv("STRATA_MANIFEST_REWRITER_DRY_RUN", "")

	r, err := buildManifestRewriter(Dependencies{Meta: metamem.New(), Data: datamem.New()})
	if err != nil {
		t.Fatalf("buildManifestRewriter: %v", err)
	}
	rr, ok := r.(*manifestRewriterRunner)
	if !ok {
		t.Fatalf("buildManifestRewriter returned %T, want *manifestRewriterRunner", r)
	}
	if rr.interval != 24*time.Hour {
		t.Errorf("interval = %v, want 24h", rr.interval)
	}
	if rr.dryRun {
		t.Errorf("dryRun = true, want false")
	}
}

func TestBuildManifestRewriterRequiresMeta(t *testing.T) {
	if _, err := buildManifestRewriter(Dependencies{}); err == nil {
		t.Fatal("buildManifestRewriter: want error for missing meta, got nil")
	}
}

func TestBoolFromEnv(t *testing.T) {
	t.Setenv("STRATA_MANIFEST_REWRITER_BOOL_TEST", "")
	if got := boolFromEnv("STRATA_MANIFEST_REWRITER_BOOL_TEST", true); got != true {
		t.Errorf("boolFromEnv unset = %v, want true", got)
	}
	t.Setenv("STRATA_MANIFEST_REWRITER_BOOL_TEST", "true")
	if got := boolFromEnv("STRATA_MANIFEST_REWRITER_BOOL_TEST", false); got != true {
		t.Errorf("boolFromEnv true = %v, want true", got)
	}
	t.Setenv("STRATA_MANIFEST_REWRITER_BOOL_TEST", "false")
	if got := boolFromEnv("STRATA_MANIFEST_REWRITER_BOOL_TEST", true); got != false {
		t.Errorf("boolFromEnv false = %v, want false", got)
	}
	t.Setenv("STRATA_MANIFEST_REWRITER_BOOL_TEST", "garbage")
	if got := boolFromEnv("STRATA_MANIFEST_REWRITER_BOOL_TEST", true); got != true {
		t.Errorf("boolFromEnv malformed = %v, want fallback true", got)
	}
}

// TestManifestRewriterRunnerIdempotentReRun exercises the supervisor's
// re-run path: the runner runs the worker once, sleeps a tiny interval,
// runs it again, and shuts down on ctx cancel. The second pass MUST
// skip the proto rows the first pass produced (idempotency AC).
func TestManifestRewriterRunnerIdempotentReRun(t *testing.T) {
	prev := data.ManifestFormat()
	t.Cleanup(func() { _ = data.SetManifestFormat(prev) })

	store := metamem.New()
	bucket, err := store.CreateBucket(context.Background(), "bkt", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}

	if err := data.SetManifestFormat(data.ManifestFormatJSON); err != nil {
		t.Fatalf("set json: %v", err)
	}
	for _, k := range []string{"a", "b"} {
		if err := store.PutObject(context.Background(), &meta.Object{
			BucketID:     bucket.ID,
			Key:          k,
			StorageClass: "STANDARD",
			ETag:         `"x"`,
			Size:         1,
			Mtime:        time.Now().UTC(),
			Manifest: &data.Manifest{
				Class: "STANDARD", Size: 1, ChunkSize: 4 * 1024 * 1024, ETag: `"x"`,
			},
		}, false); err != nil {
			t.Fatalf("put %q: %v", k, err)
		}
	}
	if err := data.SetManifestFormat(data.ManifestFormatProto); err != nil {
		t.Fatalf("set proto: %v", err)
	}

	w, err := manifestrewriter.New(manifestrewriter.Config{Meta: store, Logger: slog.Default()})
	if err != nil {
		t.Fatalf("new worker: %v", err)
	}
	runner := &manifestRewriterRunner{
		worker:   w,
		interval: 10 * time.Millisecond,
		logger:   slog.Default(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := runner.Run(ctx); err != nil {
		t.Fatalf("runner.Run: %v", err)
	}

	for _, k := range []string{"a", "b"} {
		raw, err := store.GetObjectManifestRaw(context.Background(), bucket.ID, k, "")
		if err != nil {
			t.Fatalf("get raw %q: %v", k, err)
		}
		if data.IsManifestJSON(raw) {
			t.Fatalf("post-run still JSON for %q", k)
		}
	}
}

func TestManifestRewriterRunnerStopsOnContextCancel(t *testing.T) {
	w, err := manifestrewriter.New(manifestrewriter.Config{Meta: metamem.New(), Logger: slog.Default()})
	if err != nil {
		t.Fatalf("new worker: %v", err)
	}
	runner := &manifestRewriterRunner{
		worker:   w,
		interval: time.Hour,
		logger:   slog.Default(),
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()
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
