package usagerollup

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	metamem "github.com/danchupin/strata/internal/meta/memory"
	strataotel "github.com/danchupin/strata/internal/otel"
)

func TestUsageRollupEmitsIterationAndSampleSpan(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	store := metamem.New()
	ctx := context.Background()
	if _, err := store.CreateBucket(ctx, "rollup", "alice", "STANDARD"); err != nil {
		t.Fatalf("create: %v", err)
	}
	now := time.Date(2026, 5, 10, 0, 5, 0, 0, time.UTC)
	w, err := New(Config{
		Meta:   store,
		Now:    func() time.Time { return now },
		Tracer: tp.Tracer("strata.worker.usage-rollup"),
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if _, err := w.RunOnce(ctx, now); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	spans := exp.GetSpans()
	var iter, sample *tracetest.SpanStub
	for i := range spans {
		s := &spans[i]
		switch s.Name {
		case "worker.usage-rollup.tick":
			iter = s
		case "usage_rollup.sample_bucket":
			sample = s
		}
	}
	if iter == nil {
		t.Fatalf("missing worker.usage-rollup.tick iteration span")
	}
	if !hasAttr(iter.Attributes, strataotel.AttrComponentKey, "worker") {
		t.Errorf("iteration missing strata.component=worker; got %v", iter.Attributes)
	}
	if !hasAttr(iter.Attributes, strataotel.WorkerKey, "usage-rollup") {
		t.Errorf("iteration missing strata.worker=usage-rollup; got %v", iter.Attributes)
	}
	if sample == nil {
		t.Errorf("expected usage_rollup.sample_bucket child span")
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
