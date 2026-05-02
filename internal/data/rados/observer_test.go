package rados

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/danchupin/strata/internal/logging"
)

func TestLogOpDebugEmitsExpectedFields(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx := logging.WithRequestID(context.Background(), "req-rados")

	LogOp(ctx, logger, "put", "abc.00000", 12*time.Millisecond, nil)

	out := buf.String()
	for _, want := range []string{
		`"level":"DEBUG"`,
		`"msg":"rados: op"`,
		`"request_id":"req-rados"`,
		`"op":"put"`,
		`"oid":"abc.00000"`,
		`"duration_ms":12`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("log missing %q\nfull: %s", want, out)
		}
	}
}

func TestLogOpIncludesErrorWhenSet(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	LogOp(context.Background(), logger, "get", "x", 0, errors.New("boom"))

	if !strings.Contains(buf.String(), `"error":"boom"`) {
		t.Fatalf("missing error attr; got %s", buf.String())
	}
}

func TestLogOpSilentAtInfoLevel(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	LogOp(context.Background(), logger, "del", "x", 0, nil)

	if buf.Len() != 0 {
		t.Fatalf("expected no log at INFO; got %s", buf.String())
	}
}

func TestLogOpNilLoggerIsNoop(t *testing.T) {
	// Must not panic.
	LogOp(context.Background(), nil, "put", "x", time.Millisecond, nil)
}

func TestObserveOpEmitsSpan(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSyncer(exp),
	)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	parentTracer := tp.Tracer("strata.gateway")
	ctx, parent := parentTracer.Start(context.Background(), "PUT /bkt/key")

	ObserveOp(ctx, nil, nil, tp.Tracer("strata.data.rados"),
		"rgw.data", "put", "obj.00000", time.Now().Add(-3*time.Millisecond), nil)
	parent.End()

	spans := exp.GetSpans()
	if len(spans) != 2 {
		t.Fatalf("want 2 spans (rados child + parent), got %d", len(spans))
	}
	var child *tracetest.SpanStub
	for i := range spans {
		if spans[i].Name == "data.rados.put" {
			child = &spans[i]
			break
		}
	}
	if child == nil {
		t.Fatalf("missing data.rados.put span; got %+v", spans)
	}
	if child.Parent.SpanID() != parent.SpanContext().SpanID() {
		t.Errorf("child parent=%s want %s", child.Parent.SpanID(), parent.SpanContext().SpanID())
	}
	for _, want := range [][2]string{
		{"data.rados.pool", "rgw.data"},
		{"data.rados.op", "put"},
		{"data.rados.oid", "obj.00000"},
	} {
		found := false
		for _, kv := range child.Attributes {
			if string(kv.Key) == want[0] && kv.Value.AsString() == want[1] {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing attr %s=%s; got %v", want[0], want[1], child.Attributes)
		}
	}
}

func TestObserveOpErrorSpanFlipsStatus(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSyncer(exp),
	)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	ObserveOp(context.Background(), nil, nil, tp.Tracer("t"),
		"rgw.data", "get", "obj.00000", time.Now().Add(-time.Millisecond), errors.New("boom"))

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("want 1 span, got %d", len(spans))
	}
	if spans[0].Status.Code.String() != "Error" {
		t.Errorf("status=%s want Error", spans[0].Status.Code)
	}
}

func TestObserveOpNilTracerNoop(t *testing.T) {
	// Must not panic.
	ObserveOp(context.Background(), nil, nil, nil, "p", "put", "x", time.Now(), nil)
}
