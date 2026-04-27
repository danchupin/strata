package manifestrewriter

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/meta"
	metamem "github.com/danchupin/strata/internal/meta/memory"
)

// withFormat sets the package-level encoder format for the duration of t.
func withFormat(t *testing.T, f string) {
	t.Helper()
	prev := data.ManifestFormat()
	if err := data.SetManifestFormat(f); err != nil {
		t.Fatalf("set format %q: %v", f, err)
	}
	t.Cleanup(func() { _ = data.SetManifestFormat(prev) })
}

func putObject(t *testing.T, store meta.Store, bucketID [16]byte, key string) {
	t.Helper()
	if err := store.PutObject(context.Background(), &meta.Object{
		BucketID:     bucketID,
		Key:          key,
		StorageClass: "STANDARD",
		ETag:         `"abc"`,
		Size:         3,
		Mtime:        time.Now().UTC(),
		Manifest: &data.Manifest{
			Class:     "STANDARD",
			Size:      3,
			ChunkSize: 4 * 1024 * 1024,
			ETag:      `"abc"`,
		},
	}, false); err != nil {
		t.Fatalf("put %q: %v", key, err)
	}
}

func TestPutNewObjectYieldsProtoByDefault(t *testing.T) {
	withFormat(t, data.ManifestFormatProto)
	store := metamem.New()
	b, err := store.CreateBucket(context.Background(), "bkt", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	putObject(t, store, b.ID, "k")
	raw, err := store.GetObjectManifestRaw(context.Background(), b.ID, "k", "")
	if err != nil {
		t.Fatalf("get raw: %v", err)
	}
	if data.IsManifestJSON(raw) {
		t.Fatalf("expected proto blob; first byte=%q", raw[:1])
	}
}

func TestRewriterConvertsJSONRowsToProto(t *testing.T) {
	store := metamem.New()
	b, err := store.CreateBucket(context.Background(), "bkt", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}

	// Write the rows under the JSON encoder.
	withFormat(t, data.ManifestFormatJSON)
	putObject(t, store, b.ID, "a")
	putObject(t, store, b.ID, "b")
	putObject(t, store, b.ID, "c")

	for _, k := range []string{"a", "b", "c"} {
		raw, err := store.GetObjectManifestRaw(context.Background(), b.ID, k, "")
		if err != nil {
			t.Fatalf("pre-rewrite raw %q: %v", k, err)
		}
		if !data.IsManifestJSON(raw) {
			t.Fatalf("expected JSON for %q; first byte=%q", k, raw[:1])
		}
	}

	// Flip the encoder back to proto so any new write the rewriter triggers
	// uses proto. Run the rewriter.
	withFormat(t, data.ManifestFormatProto)
	w, err := New(Config{
		Meta:   store,
		Logger: slog.Default(),
	})
	if err != nil {
		t.Fatalf("new worker: %v", err)
	}
	stats, err := w.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if stats.ObjectsScanned != 3 || stats.ObjectsRewritten != 3 || stats.ObjectsSkippedProto != 0 {
		t.Fatalf("first pass stats: %+v", stats)
	}

	for _, k := range []string{"a", "b", "c"} {
		raw, err := store.GetObjectManifestRaw(context.Background(), b.ID, k, "")
		if err != nil {
			t.Fatalf("post-rewrite raw %q: %v", k, err)
		}
		if data.IsManifestJSON(raw) {
			t.Fatalf("post-rewrite still JSON %q first byte=%q", k, raw[:1])
		}
		// Decoder still reads the proto blob into a usable Manifest.
		got, err := store.GetObject(context.Background(), b.ID, k, "")
		if err != nil {
			t.Fatalf("get %q: %v", k, err)
		}
		if got.Manifest == nil || got.Manifest.Class != "STANDARD" {
			t.Fatalf("post-rewrite manifest %q: %+v", k, got.Manifest)
		}
	}

	// Re-running is idempotent: every row is now proto so nothing rewrites.
	stats2, err := w.Run(context.Background())
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if stats2.ObjectsRewritten != 0 || stats2.ObjectsSkippedProto != 3 {
		t.Fatalf("second pass stats: %+v", stats2)
	}
}

func TestRewriterReadsBothFormats(t *testing.T) {
	store := metamem.New()
	b, err := store.CreateBucket(context.Background(), "bkt", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}

	// One row JSON, one row proto.
	withFormat(t, data.ManifestFormatJSON)
	putObject(t, store, b.ID, "json-row")
	withFormat(t, data.ManifestFormatProto)
	putObject(t, store, b.ID, "proto-row")

	w, err := New(Config{Meta: store, Logger: slog.Default()})
	if err != nil {
		t.Fatalf("new worker: %v", err)
	}
	stats, err := w.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if stats.ObjectsScanned != 2 || stats.ObjectsRewritten != 1 || stats.ObjectsSkippedProto != 1 {
		t.Fatalf("mixed-format stats: %+v", stats)
	}
}

func TestRewriterDryRunDoesNotMutate(t *testing.T) {
	store := metamem.New()
	b, err := store.CreateBucket(context.Background(), "bkt", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	withFormat(t, data.ManifestFormatJSON)
	putObject(t, store, b.ID, "k")

	withFormat(t, data.ManifestFormatProto)
	w, err := New(Config{Meta: store, Logger: slog.Default(), DryRun: true})
	if err != nil {
		t.Fatalf("new worker: %v", err)
	}
	stats, err := w.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if stats.ObjectsRewritten != 1 {
		t.Fatalf("dry-run should still count rewrites: %+v", stats)
	}
	raw, err := store.GetObjectManifestRaw(context.Background(), b.ID, "k", "")
	if err != nil {
		t.Fatalf("post dry-run raw: %v", err)
	}
	if !data.IsManifestJSON(raw) {
		t.Fatalf("dry-run mutated the row; first byte=%q", raw[:1])
	}
}
