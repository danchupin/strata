package otel

import (
	"context"
	"errors"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestStartIterationEmitsParentSpan(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	tracer := tp.Tracer("strata.worker.gc")
	ctx, span := StartIteration(context.Background(), tracer, "gc")
	EndIteration(span, nil)

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("want 1 span, got %d", len(spans))
	}
	s := spans[0]
	if s.Name != "worker.gc.tick" {
		t.Errorf("span name = %q, want worker.gc.tick", s.Name)
	}
	if !hasAttr(s.Attributes, AttrComponentKey, "worker") {
		t.Errorf("missing strata.component=worker; got %v", s.Attributes)
	}
	if !hasAttr(s.Attributes, WorkerKey, "gc") {
		t.Errorf("missing strata.worker=gc; got %v", s.Attributes)
	}
	var foundID bool
	for _, kv := range s.Attributes {
		if string(kv.Key) == IterationIDKey {
			foundID = true
			if kv.Value.AsInt64() <= 0 {
				t.Errorf("iteration_id = %d, want > 0", kv.Value.AsInt64())
			}
		}
	}
	if !foundID {
		t.Errorf("missing %s attribute", IterationIDKey)
	}
	_ = ctx
}

func TestStartIterationCounterIsMonotonic(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	tracer := tp.Tracer("strata.worker.iter")
	_, a := StartIteration(context.Background(), tracer, "iter-test")
	EndIteration(a, nil)
	_, b := StartIteration(context.Background(), tracer, "iter-test")
	EndIteration(b, nil)

	spans := exp.GetSpans()
	if len(spans) != 2 {
		t.Fatalf("want 2 spans, got %d", len(spans))
	}
	var ids []int64
	for _, s := range spans {
		for _, kv := range s.Attributes {
			if string(kv.Key) == IterationIDKey {
				ids = append(ids, kv.Value.AsInt64())
			}
		}
	}
	if len(ids) != 2 {
		t.Fatalf("want 2 iteration ids, got %v", ids)
	}
	if ids[1] != ids[0]+1 {
		t.Errorf("iteration ids not monotonic: %v", ids)
	}
}

func TestEndIterationMarksError(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	tracer := tp.Tracer("strata.worker.gc")
	_, span := StartIteration(context.Background(), tracer, "gc-err")
	EndIteration(span, errors.New("boom"))

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("want 1 span, got %d", len(spans))
	}
	if spans[0].Status.Code.String() != "Error" {
		t.Errorf("status = %s, want Error", spans[0].Status.Code)
	}
	if spans[0].Status.Description != "boom" {
		t.Errorf("status description = %q, want boom", spans[0].Status.Description)
	}
	if len(spans[0].Events) == 0 {
		t.Errorf("expected RecordError event, got none")
	}
}

func TestStartIterationNilTracerNoop(t *testing.T) {
	_, span := StartIteration(context.Background(), nil, "nil-tracer")
	if span == nil {
		t.Fatalf("StartIteration returned nil span with nil tracer")
	}
	// No panic on End is the contract.
	EndIteration(span, nil)
	EndIteration(span, errors.New("x"))
}

func TestNoopTracerReturned(t *testing.T) {
	if NoopTracer() == nil {
		t.Fatalf("NoopTracer() returned nil")
	}
	_, span := NoopTracer().Start(context.Background(), "x")
	span.End()
}
