package s3

import (
	"context"
	"errors"
	"strings"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/danchupin/strata/internal/data"
)

// TestOtelawsStampsStrataAttributes pins the US-003 acceptance: driving a
// PutObject through the SDK with the otelaws middleware installed emits
// one client-kind span carrying strata.component=gateway,
// strata.s3_cluster=<id>, and the otelaws-default rpc.method attribute
// shaped `<service>/<operation>` (semconv v1.40 merged rpc.service into
// rpc.method).
func TestOtelawsStampsStrataAttributes(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSyncer(exp),
	)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	seq := &sequenceTransport{
		responses: []responseFn{putObjectSuccessResponse},
	}
	b := openTestBackend(t, seq, func(c *Config) {
		c.Tracer = tp.Tracer("strata.data.s3")
		c.TracerProvider = tp
	})

	if _, err := b.Put(context.Background(), "k", strings.NewReader("payload"), 7); err != nil {
		t.Fatalf("Put: %v", err)
	}

	spans := exp.GetSpans()
	if len(spans) == 0 {
		t.Fatalf("no spans emitted; want at least one PutObject span")
	}
	var put *tracetest.SpanStub
	for i := range spans {
		if strings.Contains(spans[i].Name, "PutObject") {
			put = &spans[i]
			break
		}
	}
	if put == nil {
		names := make([]string, len(spans))
		for i := range spans {
			names[i] = spans[i].Name
		}
		t.Fatalf("missing PutObject span; got %v", names)
	}

	requireAttr(t, put, "strata.component", "gateway")
	requireAttr(t, put, "strata.s3_cluster", "default")
	requireAttrContains(t, put, "rpc.method", "PutObject")
	// otelaws emits semconv v1.40 attribute name `rpc.system.name` (renamed
	// from `rpc.system`); pin the present name so a future semconv bump
	// surfaces here loudly.
	requireAttr(t, put, "rpc.system.name", "aws-api")

	if put.SpanKind.String() != "client" {
		t.Errorf("span kind=%s want client", put.SpanKind)
	}
}

// TestOtelawsErrorPathFlipsStatus drives a 404 NoSuchKey through GetObject
// and asserts the otelaws-emitted span lands with Error status so the
// tail-sampler always exports it.
func TestOtelawsErrorPathFlipsStatus(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSyncer(exp),
	)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	seq := &sequenceTransport{
		responses: []responseFn{noSuchKeyResponse},
	}
	b := openTestBackend(t, seq, func(c *Config) {
		c.Tracer = tp.Tracer("strata.data.s3")
		c.TracerProvider = tp
	})

	_, err := b.Get(context.Background(), "missing")
	if err == nil {
		t.Fatal("Get: expected NoSuchKey error")
	}
	if !errors.Is(err, data.ErrNotFound) {
		t.Fatalf("Get: want data.ErrNotFound, got %v", err)
	}

	spans := exp.GetSpans()
	var get *tracetest.SpanStub
	for i := range spans {
		if strings.Contains(spans[i].Name, "GetObject") {
			get = &spans[i]
			break
		}
	}
	if get == nil {
		t.Fatalf("missing GetObject span; got %d spans", len(spans))
	}
	if get.Status.Code.String() != "Error" {
		t.Errorf("status=%s want Error", get.Status.Code)
	}
}

// TestNilTracerSkipsMiddleware proves the zero-config test fixture path:
// when cfg.Tracer is nil + cfg.TracerProvider is nil, no otelaws spans
// are emitted from the test exporter — middleware install is skipped so
// existing tests keep their hermetic shape.
func TestNilTracerSkipsMiddleware(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSyncer(exp),
	)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	seq := &sequenceTransport{
		responses: []responseFn{putObjectSuccessResponse},
	}
	// openTestBackend leaves Tracer + TracerProvider nil by default.
	b := openTestBackend(t, seq)
	if _, err := b.Put(context.Background(), "k", strings.NewReader("payload"), 7); err != nil {
		t.Fatalf("Put: %v", err)
	}

	if got := len(exp.GetSpans()); got != 0 {
		t.Fatalf("want 0 spans (middleware not installed), got %d", got)
	}
}

func requireAttr(t *testing.T, span *tracetest.SpanStub, key, want string) {
	t.Helper()
	for _, kv := range span.Attributes {
		if string(kv.Key) == key {
			if got := kv.Value.AsString(); got != want {
				t.Errorf("attr %s=%q want %q", key, got, want)
			}
			return
		}
	}
	t.Errorf("missing attr %s; got %v", key, span.Attributes)
}

func requireAttrContains(t *testing.T, span *tracetest.SpanStub, key, want string) {
	t.Helper()
	for _, kv := range span.Attributes {
		if string(kv.Key) == key {
			if got := kv.Value.AsString(); !strings.Contains(got, want) {
				t.Errorf("attr %s=%q does not contain %q", key, got, want)
			}
			return
		}
	}
	t.Errorf("missing attr %s; got %v", key, span.Attributes)
}

