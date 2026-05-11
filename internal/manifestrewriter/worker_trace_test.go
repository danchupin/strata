package manifestrewriter

import (
	"context"
	"log/slog"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	metamem "github.com/danchupin/strata/internal/meta/memory"
	strataotel "github.com/danchupin/strata/internal/otel"
)

func TestManifestRewriterEmitsIterationAndRewriteSpan(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	store := metamem.New()
	b, err := store.CreateBucket(context.Background(), "bkt", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	putObject(t, store, b.ID, "k")

	w, err := New(Config{
		Meta:   store,
		Logger: slog.Default(),
		Tracer: tp.Tracer("strata.worker.manifest-rewriter"),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := w.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	spans := exp.GetSpans()
	var iter, rewrite *tracetest.SpanStub
	for i := range spans {
		s := &spans[i]
		switch s.Name {
		case "worker.manifest-rewriter.tick":
			iter = s
		case "manifest_rewriter.rewrite_bucket":
			rewrite = s
		}
	}
	if iter == nil {
		t.Fatalf("missing worker.manifest-rewriter.tick iteration span")
	}
	if !hasAttr(iter.Attributes, strataotel.AttrComponentKey, "worker") {
		t.Errorf("iteration missing strata.component=worker; got %v", iter.Attributes)
	}
	if !hasAttr(iter.Attributes, strataotel.WorkerKey, "manifest-rewriter") {
		t.Errorf("iteration missing strata.worker=manifest-rewriter; got %v", iter.Attributes)
	}
	if rewrite == nil {
		t.Errorf("expected manifest_rewriter.rewrite_bucket child span")
	}
}

func hasAttr(attrs []attribute.KeyValue, key, val string) bool {
	for _, kv := range attrs {
		if string(kv.Key) == key && kv.Value.AsString() == val {
			return true
		}
	}
	return false
}
