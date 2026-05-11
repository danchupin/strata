package workers

import (
	"context"

	"go.opentelemetry.io/otel/trace"

	strataotel "github.com/danchupin/strata/internal/otel"
)

// StartIteration emits the per-iteration parent span used by background
// workers. Thin re-export of `strataotel.StartIteration` so cmd-layer call
// sites match the convention documented in the PRD without having to
// import internal/otel directly.
//
// Span shape: name=`worker.<workerName>.tick`, attributes
// `strata.component=worker`, `strata.worker=<workerName>`,
// `strata.iteration_id=<atomic-uint64>`. If tracer is nil the helper
// substitutes a no-op tracer.
func StartIteration(ctx context.Context, tracer trace.Tracer, workerName string) (context.Context, trace.Span) {
	return strataotel.StartIteration(ctx, tracer, workerName)
}

// EndIteration finishes the iteration span. Marks status=Error and records
// err when non-nil so the tail-sampler always exports failing iterations.
func EndIteration(span trace.Span, err error) {
	strataotel.EndIteration(span, err)
}
