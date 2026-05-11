package otel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestMiddlewareStampsGatewayComponent(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	prev := otel.GetTracerProvider()
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	p, err := InitWithConfig(context.Background(), Config{
		Exporter:    exp,
		SampleRatio: 1.0,
		ServiceName: "test",
	})
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })

	handler := NewMiddleware(p, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/bkt/key", nil)
	handler.ServeHTTP(rec, req)

	if err := p.ForceFlush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}
	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("want 1 span, got %d", len(spans))
	}
	if !hasAttr(spans[0].Attributes, AttrComponentKey, "gateway") {
		t.Errorf("missing %s=gateway; got %v", AttrComponentKey, spans[0].Attributes)
	}
}
