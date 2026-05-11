package notify

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	strataotel "github.com/danchupin/strata/internal/otel"
)

func TestNotifyWorkerEmitsIterationAndDeliverSpan(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	sink := &fakeSink{name: "fake-sink"}
	w, store, b := newWorkerHarness(t, sink, func(c *Config) {
		c.Tracer = tp.Tracer("strata.worker.notify")
	})
	_ = enqueueEvent(t, store, b, "evt-1")

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	spans := exp.GetSpans()
	if len(spans) == 0 {
		t.Fatalf("no spans emitted")
	}
	var (
		iter    *tracetest.SpanStub
		deliver *tracetest.SpanStub
	)
	for i := range spans {
		s := &spans[i]
		switch s.Name {
		case "worker.notify.tick":
			iter = s
		case "notify.deliver_event":
			deliver = s
		}
	}
	if iter == nil {
		t.Fatalf("missing worker.notify.tick iteration span")
	}
	if !hasAttr(iter.Attributes, strataotel.AttrComponentKey, "worker") {
		t.Errorf("iteration missing strata.component=worker; got %v", iter.Attributes)
	}
	if !hasAttr(iter.Attributes, strataotel.WorkerKey, "notify") {
		t.Errorf("iteration missing strata.worker=notify; got %v", iter.Attributes)
	}
	if deliver == nil {
		t.Errorf("expected notify.deliver_event child span")
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
