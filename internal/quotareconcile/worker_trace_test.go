package quotareconcile

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

func TestQuotaReconcileEmitsIterationAndScanSpan(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	store := metamem.New()
	b := mustCreateBucket(t, store, "bkt")
	mustPutObject(t, store, b.ID, "a", 1000)

	w, err := New(Config{
		Meta:   store,
		Logger: slog.Default(),
		Tracer: tp.Tracer("strata.worker.quota-reconcile"),
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if _, err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	spans := exp.GetSpans()
	var iter, scan *tracetest.SpanStub
	for i := range spans {
		s := &spans[i]
		switch s.Name {
		case "worker.quota-reconcile.tick":
			iter = s
		case "quota_reconcile.scan_bucket":
			scan = s
		}
	}
	if iter == nil {
		t.Fatalf("missing worker.quota-reconcile.tick iteration span")
	}
	if !hasAttr(iter.Attributes, strataotel.AttrComponentKey, "worker") {
		t.Errorf("iteration missing strata.component=worker; got %v", iter.Attributes)
	}
	if !hasAttr(iter.Attributes, strataotel.WorkerKey, "quota-reconcile") {
		t.Errorf("iteration missing strata.worker=quota-reconcile; got %v", iter.Attributes)
	}
	if scan == nil {
		t.Errorf("expected quota_reconcile.scan_bucket child span")
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
