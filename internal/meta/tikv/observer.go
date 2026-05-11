package tikv

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	strataotel "github.com/danchupin/strata/internal/otel"
)

// Observer emits one client-kind child span per Store-level TiKV operation.
// Mirrors the cassandra SlowQueryObserver shape but driven explicitly by the
// Store methods because TiKV has no gocql-style per-query observer hook.
type Observer struct {
	tracer trace.Tracer
}

// NewObserver returns nil when tracer is nil so callers can construct without
// a guard. A nil Observer is a no-op for both WrapOp and Start.
func NewObserver(tracer trace.Tracer) *Observer {
	if tracer == nil {
		return nil
	}
	return &Observer{tracer: tracer}
}

// WrapOp runs fn inside a meta.tikv.<table>.<op> client-kind span. When the
// observer is nil or has no tracer, fn runs directly without span emission.
// Failing ops mark the span Error so the tail-sampler always exports them.
func (o *Observer) WrapOp(ctx context.Context, op, table string, fn func(ctx context.Context) error) error {
	if o == nil || o.tracer == nil {
		return fn(ctx)
	}
	spanCtx, end := o.start(ctx, op, table)
	err := fn(spanCtx)
	end(err)
	return err
}

// Start returns a child-span context plus an end-function the caller invokes
// via defer when wrapping a method body with named-return-driven error
// reporting. When the observer is nil or has no tracer, Start is identity
// (returns the original ctx and a no-op end-function).
func (o *Observer) Start(ctx context.Context, op, table string) (context.Context, func(error)) {
	if o == nil || o.tracer == nil {
		return ctx, func(error) {}
	}
	return o.start(ctx, op, table)
}

func (o *Observer) start(ctx context.Context, op, table string) (context.Context, func(error)) {
	tableLabel := table
	if tableLabel == "" {
		tableLabel = "unknown"
	}
	opLabel := op
	if opLabel == "" {
		opLabel = "UNKNOWN"
	}
	spanCtx, span := o.tracer.Start(ctx, "meta.tikv."+tableLabel+"."+opLabel,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			strataotel.AttrComponentGateway,
			attribute.String("db.system", "tikv"),
			attribute.String("db.tikv.table", tableLabel),
			attribute.String("db.operation", opLabel),
		),
	)
	return spanCtx, func(err error) {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		span.End()
	}
}
