package tikv

import (
	"context"
	"errors"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func newTestTracerProvider(t *testing.T) (*sdktrace.TracerProvider, *tracetest.InMemoryExporter) {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSyncer(exp),
	)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	return tp, exp
}

func TestObserverWrapOpEmitsSpan(t *testing.T) {
	tp, exp := newTestTracerProvider(t)
	obs := NewObserver(tp.Tracer("strata.meta.tikv"))

	parent := tp.Tracer("strata.gateway")
	ctx, top := parent.Start(context.Background(), "PUT /bkt/key")
	err := obs.WrapOp(ctx, "PutObject", "objects", func(ctx context.Context) error {
		return nil
	})
	top.End()
	if err != nil {
		t.Fatalf("WrapOp err: %v", err)
	}

	spans := exp.GetSpans()
	if len(spans) != 2 {
		t.Fatalf("want 2 spans, got %d", len(spans))
	}
	var child *tracetest.SpanStub
	for i := range spans {
		if spans[i].Name == "meta.tikv.objects.PutObject" {
			child = &spans[i]
			break
		}
	}
	if child == nil {
		t.Fatalf("missing meta.tikv.objects.PutObject span; got %+v", spans)
	}
	if child.Parent.SpanID() != top.SpanContext().SpanID() {
		t.Errorf("child parent=%s want %s", child.Parent.SpanID(), top.SpanContext().SpanID())
	}
	if child.SpanKind != trace.SpanKindClient {
		t.Errorf("child kind=%v want client", child.SpanKind)
	}
	for _, want := range [][2]string{
		{"db.system", "tikv"},
		{"db.tikv.table", "objects"},
		{"db.operation", "PutObject"},
		{"strata.component", "gateway"},
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
	if child.Status.Code.String() != "Unset" {
		t.Errorf("status=%v want Unset", child.Status.Code)
	}
}

func TestObserverWrapOpErrorFlipsStatus(t *testing.T) {
	tp, exp := newTestTracerProvider(t)
	obs := NewObserver(tp.Tracer("strata.meta.tikv"))

	wantErr := errors.New("boom")
	err := obs.WrapOp(context.Background(), "GetObject", "objects", func(ctx context.Context) error {
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("err=%v want %v", err, wantErr)
	}

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("want 1 span, got %d", len(spans))
	}
	if spans[0].Status.Code.String() != "Error" {
		t.Errorf("status=%v want Error", spans[0].Status.Code)
	}
	if spans[0].Status.Description != wantErr.Error() {
		t.Errorf("status desc=%q want %q", spans[0].Status.Description, wantErr.Error())
	}
}

func TestObserverNilTracerIsNoop(t *testing.T) {
	if obs := NewObserver(nil); obs != nil {
		t.Fatalf("NewObserver(nil) should return nil, got %v", obs)
	}
	var obs *Observer
	if err := obs.WrapOp(context.Background(), "X", "y", func(context.Context) error { return nil }); err != nil {
		t.Fatalf("nil observer WrapOp err: %v", err)
	}
	ctx, end := obs.Start(context.Background(), "X", "y")
	if ctx == nil {
		t.Fatalf("nil observer Start returned nil ctx")
	}
	end(nil)
}

func TestStoreWiresObserverPutObject(t *testing.T) {
	tp, exp := newTestTracerProvider(t)
	obs := NewObserver(tp.Tracer("strata.meta.tikv"))
	s := openWithBackendAndObserver(newMemBackend(), obs)

	if _, err := s.CreateBucket(context.Background(), "bkt", "owner", "STANDARD"); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}

	spans := exp.GetSpans()
	var match *tracetest.SpanStub
	for i := range spans {
		if spans[i].Name == "meta.tikv.buckets.CreateBucket" {
			match = &spans[i]
			break
		}
	}
	if match == nil {
		t.Fatalf("missing meta.tikv.buckets.CreateBucket span; got %+v", spans)
	}
}
