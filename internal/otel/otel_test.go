package otel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

func TestRatioFromEnvDefaults(t *testing.T) {
	t.Setenv(EnvSampleRatio, "")
	if got := ratioFromEnv(); got != DefaultSampleRatio {
		t.Errorf("default ratio = %v, want %v", got, DefaultSampleRatio)
	}
}

func TestRatioFromEnvParses(t *testing.T) {
	cases := map[string]float64{
		"0":      0,
		"0.5":    0.5,
		"1":      1,
		"1.5":    1,    // clamped
		"-1":     DefaultSampleRatio, // bad → default
		"banana": DefaultSampleRatio, // bad → default
	}
	for in, want := range cases {
		t.Setenv(EnvSampleRatio, in)
		if got := ratioFromEnv(); got != want {
			t.Errorf("ratio(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestInitNoEndpointInstallsNoopProvider(t *testing.T) {
	t.Setenv(EnvEndpoint, "")
	t.Setenv(EnvSampleRatio, "")

	prev := otel.GetTracerProvider()
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	p, err := Init(context.Background())
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	defer func() { _ = p.Shutdown(context.Background()) }()

	if p.tp != nil {
		t.Errorf("expected no SDK provider when endpoint empty")
	}
	tp := otel.GetTracerProvider()
	if _, ok := tp.(tracenoop.TracerProvider); !ok {
		// fine — different concrete noop implementation may exist; check
		// behaviourally instead.
		_, span := tp.Tracer("t").Start(context.Background(), "x")
		if span.SpanContext().IsSampled() {
			t.Errorf("expected unsampled span from noop provider")
		}
		span.End()
	}
}

func TestInitWithExporterEmitsOneSpanPerRequest(t *testing.T) {
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
	defer func() { _ = p.Shutdown(context.Background()) }()

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
	got := spans[0]
	if got.Name != "GET /bkt/key" {
		t.Errorf("span name = %q", got.Name)
	}
	if !hasAttr(got.Attributes, "http.method", "GET") {
		t.Errorf("missing http.method attr; got %v", got.Attributes)
	}
	if !hasIntAttr(got.Attributes, "http.status_code", 200) {
		t.Errorf("missing http.status_code=200; got %v", got.Attributes)
	}
}

func TestMiddlewareExtractsTraceparent(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	prev := otel.GetTracerProvider()
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	p, err := InitWithConfig(context.Background(), Config{
		Exporter: exp, SampleRatio: 1.0, ServiceName: "test",
	})
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	defer func() { _ = p.Shutdown(context.Background()) }()

	handler := NewMiddleware(p, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	const tp = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("traceparent", tp)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if err := p.ForceFlush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}
	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("want 1 span, got %d", len(spans))
	}
	if got := spans[0].SpanContext.TraceID().String(); got != "0af7651916cd43dd8448eb211c80319c" {
		t.Errorf("TraceID = %s; want propagated", got)
	}
}

func TestSamplingZeroRatioDropsSuccessSpans(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	prev := otel.GetTracerProvider()
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	p, err := InitWithConfig(context.Background(), Config{
		Exporter: exp, SampleRatio: 0, ServiceName: "test",
	})
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	defer func() { _ = p.Shutdown(context.Background()) }()

	handler := NewMiddleware(p, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for i := 0; i < 50; i++ {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		handler.ServeHTTP(httptest.NewRecorder(), req)
	}

	if err := p.ForceFlush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if got := len(exp.GetSpans()); got != 0 {
		t.Fatalf("ratio=0 should drop all success spans, got %d", got)
	}
}

func TestSamplingErrorAlwaysExportsAtZeroRatio(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	prev := otel.GetTracerProvider()
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	p, err := InitWithConfig(context.Background(), Config{
		Exporter: exp, SampleRatio: 0, ServiceName: "test",
	})
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	defer func() { _ = p.Shutdown(context.Background()) }()

	handler := NewMiddleware(p, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))

	req := httptest.NewRequest(http.MethodPost, "/oops", nil)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if err := p.ForceFlush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}
	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 error span exported even at ratio=0, got %d", len(spans))
	}
	if !hasIntAttr(spans[0].Attributes, "http.status_code", 500) {
		t.Errorf("missing http.status_code=500 attr")
	}
}

func TestSamplingFullRatioExportsAll(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	prev := otel.GetTracerProvider()
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	p, err := InitWithConfig(context.Background(), Config{
		Exporter: exp, SampleRatio: 1.0, ServiceName: "test",
	})
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	defer func() { _ = p.Shutdown(context.Background()) }()

	handler := NewMiddleware(p, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	const N = 10
	for i := 0; i < N; i++ {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	}
	if err := p.ForceFlush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if got := len(exp.GetSpans()); got != N {
		t.Errorf("want %d spans at ratio=1, got %d", N, got)
	}
}

func TestNoEndpointMiddlewareEmitsNoSpansViaExporter(t *testing.T) {
	// When Init is called with no endpoint and no exporter, the middleware
	// must still serve traffic but not produce spans through any backend
	// (the noop tracer provider returns non-recording spans).
	t.Setenv(EnvEndpoint, "")
	t.Setenv(EnvSampleRatio, "")
	prev := otel.GetTracerProvider()
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	p, err := Init(context.Background())
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	defer func() { _ = p.Shutdown(context.Background()) }()

	called := false
	handler := NewMiddleware(p, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if !called {
		t.Fatal("inner handler not invoked")
	}
	// p.tp is nil → ForceFlush is a no-op; nothing to assert beyond not
	// panicking and the request reaching the handler.
}

func hasAttr(attrs []attribute.KeyValue, key, val string) bool {
	for _, kv := range attrs {
		if string(kv.Key) == key && kv.Value.AsString() == val {
			return true
		}
	}
	return false
}

func hasIntAttr(attrs []attribute.KeyValue, key string, val int64) bool {
	for _, kv := range attrs {
		if string(kv.Key) == key && kv.Value.AsInt64() == val {
			return true
		}
	}
	return false
}
