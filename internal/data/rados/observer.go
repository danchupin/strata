package rados

import (
	"context"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/danchupin/strata/internal/logging"
	strataotel "github.com/danchupin/strata/internal/otel"
)

// LogOp emits a DEBUG log line for one RADOS operation. Cheap when logger is
// nil or the slog handler is filtered above DEBUG. Used by the ceph-tagged
// backend after each rados read/write/delete.
func LogOp(ctx context.Context, logger *slog.Logger, op, oid string, dur time.Duration, err error) {
	if logger == nil {
		return
	}
	if !logger.Enabled(ctx, slog.LevelDebug) {
		return
	}
	attrs := []any{
		"request_id", logging.RequestIDFromContext(ctx),
		"op", op,
		"oid", oid,
		"duration_ms", dur.Milliseconds(),
	}
	if err != nil {
		attrs = append(attrs, "error", err.Error())
	}
	logger.DebugContext(ctx, "rados: op", attrs...)
}

// ObserveOp fans out one RADOS op to logger (DEBUG line via LogOp), the
// Metrics interface (ObserveOp), and the OTel tracer (one child span named
// "data.rados.<op>" timestamped to (start, time.Now())). Any side may be
// nil. Pool labels the metrics histogram and the span; OID stays in the
// log line and on the span.
func ObserveOp(ctx context.Context, logger *slog.Logger, m Metrics, tracer trace.Tracer, pool, op, oid string, start time.Time, err error) {
	end := time.Now()
	dur := end.Sub(start)
	LogOp(ctx, logger, op, oid, dur, err)
	if m != nil {
		m.ObserveOp(pool, op, dur, err)
	}
	if tracer != nil {
		_, span := tracer.Start(ctx, "data.rados."+op,
			trace.WithSpanKind(trace.SpanKindClient),
			trace.WithTimestamp(start),
			trace.WithAttributes(
				strataotel.AttrComponentGateway,
				attribute.String("data.rados.pool", pool),
				attribute.String("data.rados.op", op),
				attribute.String("data.rados.oid", oid),
			),
		)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		span.End(trace.WithTimestamp(end))
	}
}
