package otel

import (
	"context"
	"sync"
	"sync/atomic"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// IterationIDKey is the attribute key stamped on every per-iteration worker
// span. Monotonic per-worker so an operator can filter for a single
// iteration in a Jaeger waterfall.
const IterationIDKey = "strata.iteration_id"

// WorkerKey is the attribute key carrying the worker's registered name
// (`gc`, `lifecycle`, …) so per-worker filters are uniform.
const WorkerKey = "strata.worker"

var (
	iterationMu       sync.Mutex
	iterationCounters = map[string]*atomic.Uint64{}
	sharedNoopTracer  = tracenoop.NewTracerProvider().Tracer("strata.noop")
)

// NoopTracer returns a process-shared no-op tracer. Observers and workers
// that may run without OTel wiring use it as a safe fallback so callers
// never have to nil-check before `tracer.Start(...)`.
func NoopTracer() trace.Tracer { return sharedNoopTracer }

// StartIteration emits the per-iteration parent span used by background
// workers. The span is named "worker.<workerName>.tick" and carries
// `strata.component=worker`, `strata.worker=<workerName>`, and a monotonic
// `strata.iteration_id` (per-worker `atomic.Uint64`).
//
// If tracer is nil a no-op tracer is substituted so callers without OTel
// wiring stay safe. The returned context carries the parent span; sub-op
// spans (`gc.scan_partition`, `lifecycle.expire_object`, …) emit beneath
// it via `tracer.Start(ctx, ...)`.
func StartIteration(ctx context.Context, tracer trace.Tracer, workerName string) (context.Context, trace.Span) {
	if tracer == nil {
		tracer = sharedNoopTracer
	}
	id := iterationCounter(workerName).Add(1)
	return tracer.Start(ctx, "worker."+workerName+".tick",
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(
			AttrComponentWorker,
			attribute.String(WorkerKey, workerName),
			attribute.Int64(IterationIDKey, int64(id)),
		),
	)
}

// EndIteration finishes the iteration span. When err != nil the span is
// marked status=Error and the error is recorded so the tail-sampler always
// exports the failing iteration regardless of `STRATA_OTEL_SAMPLE_RATIO`.
func EndIteration(span trace.Span, err error) {
	if span == nil {
		return
	}
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()
}

func iterationCounter(name string) *atomic.Uint64 {
	iterationMu.Lock()
	defer iterationMu.Unlock()
	c, ok := iterationCounters[name]
	if !ok {
		c = new(atomic.Uint64)
		iterationCounters[name] = c
	}
	return c
}
