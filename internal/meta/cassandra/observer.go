package cassandra

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gocql/gocql"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/danchupin/strata/internal/logging"
)

// EnvSlowQueryMS controls the WARN threshold for SlowQueryObserver. Default 100ms.
const EnvSlowQueryMS = "STRATA_CASSANDRA_SLOW_MS"

// DefaultSlowQueryMS is the default WARN threshold when STRATA_CASSANDRA_SLOW_MS is unset.
const DefaultSlowQueryMS = 100

// Metrics is the narrow callback the QueryObserver fans out to so the
// cassandra package never imports prometheus directly. The cmd binary plugs in
// an adapter (e.g. metrics.CassandraObserver) when registering metrics.
//
// IncLWTConflict is called by the Store at LWT call sites that observe
// applied=false. The (bucket, shard) labels back the Hot Shards heatmap
// (US-009). bucket carries the bucket name when resolvable, else the
// bucket-id UUID string.
type Metrics interface {
	ObserveQuery(table, op string, duration time.Duration, err error)
	IncLWTConflict(table, bucket, shard string)
}

// SlowQueryObserver implements gocql.QueryObserver. Queries that exceed
// Threshold (or fail with an error) are logged at WARN with structured
// attributes including request_id, table, op, duration_ms, statement. When
// Metrics is set every query (slow or not) is also reported to the metrics
// adapter so dashboards see the full latency distribution. When Tracer is
// set, each query produces a child span "meta.cassandra.<table>.<op>"
// timestamped to (q.Start, q.End) parented to the span carried by ctx.
type SlowQueryObserver struct {
	Logger    *slog.Logger
	Threshold time.Duration
	Metrics   Metrics
	Tracer    trace.Tracer
}

// NewSlowQueryObserver returns nil when there is nothing to do — no logger
// (or zero threshold) AND no metrics sink. When at least one side is enabled
// the observer is created and the disabled side is silently skipped per call.
func NewSlowQueryObserver(logger *slog.Logger, threshold time.Duration) *SlowQueryObserver {
	if logger == nil || threshold <= 0 {
		return nil
	}
	return &SlowQueryObserver{Logger: logger, Threshold: threshold}
}

// NewQueryObserver builds an observer that fans out to a logger (slow + error
// queries only), a metrics sink (every query), and an OTel tracer (one child
// span per query). Returns nil when all three are disabled. Any side may be
// nil.
func NewQueryObserver(logger *slog.Logger, threshold time.Duration, m Metrics, tracer trace.Tracer) *SlowQueryObserver {
	logEnabled := logger != nil && threshold > 0
	if !logEnabled && m == nil && tracer == nil {
		return nil
	}
	o := &SlowQueryObserver{Metrics: m, Tracer: tracer}
	if logEnabled {
		o.Logger = logger
		o.Threshold = threshold
	}
	return o
}

// SlowMSFromEnv reads STRATA_CASSANDRA_SLOW_MS. Empty/invalid -> default (100ms).
// A literal "0" disables (returns 0).
func SlowMSFromEnv() int {
	v := strings.TrimSpace(os.Getenv(EnvSlowQueryMS))
	if v == "" {
		return DefaultSlowQueryMS
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return DefaultSlowQueryMS
	}
	return n
}

// ObserveQuery is called by gocql for every query. The metrics sink (if set)
// records every observation; the logger emits WARN only for slow or failed
// queries; the tracer (if set) emits one child span per query.
func (o *SlowQueryObserver) ObserveQuery(ctx context.Context, q gocql.ObservedQuery) {
	dur := q.End.Sub(q.Start)
	table, op := parseStatement(q.Statement)
	if o.Metrics != nil {
		o.Metrics.ObserveQuery(table, op, dur, q.Err)
	}
	if o.Tracer != nil {
		o.emitSpan(ctx, q, table, op)
	}
	if o.Logger == nil || o.Threshold <= 0 {
		return
	}
	if q.Err == nil && dur < o.Threshold {
		return
	}
	attrs := []any{
		"request_id", logging.RequestIDFromContext(ctx),
		"table", table,
		"op", op,
		"duration_ms", dur.Milliseconds(),
		"statement", truncateStatement(q.Statement),
	}
	if q.Err != nil {
		attrs = append(attrs, "error", q.Err.Error())
	}
	o.Logger.WarnContext(ctx, "cassandra: slow query", attrs...)
}

// RecordLWTConflict is called by the Store when an LWT (compare-and-set)
// query returned applied=false. It fans out to the Metrics sink (so the
// CassandraLWTConflictsTotal counter on the prometheus side gets the
// {table, bucket, shard} labels needed by the Hot Shards heatmap, US-009).
// No-op when Metrics is nil.
func (o *SlowQueryObserver) RecordLWTConflict(_ context.Context, table, bucket, shard string) {
	if o == nil || o.Metrics == nil {
		return
	}
	o.Metrics.IncLWTConflict(table, bucket, shard)
}

// emitSpan creates a child span timestamped to (q.Start, q.End). Span name
// follows "meta.cassandra.<table>.<op>" (table falls back to "unknown" when
// parseStatement could not extract one). Failing queries flip span status to
// Error so the tail-sampler always exports them.
func (o *SlowQueryObserver) emitSpan(ctx context.Context, q gocql.ObservedQuery, table, op string) {
	tableLabel := table
	if tableLabel == "" {
		tableLabel = "unknown"
	}
	opLabel := op
	if opLabel == "" {
		opLabel = "UNKNOWN"
	}
	_, span := o.Tracer.Start(ctx, "meta.cassandra."+tableLabel+"."+opLabel,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithTimestamp(q.Start),
		trace.WithAttributes(
			attribute.String("db.system", "cassandra"),
			attribute.String("db.cassandra.table", tableLabel),
			attribute.String("db.operation", opLabel),
			attribute.String("db.statement", truncateStatement(q.Statement)),
		),
	)
	if q.Err != nil {
		span.RecordError(q.Err)
		span.SetStatus(codes.Error, q.Err.Error())
	}
	span.End(trace.WithTimestamp(q.End))
}

// parseStatement extracts (table, op) from a CQL statement. Best-effort.
func parseStatement(stmt string) (table, op string) {
	f := strings.Fields(stmt)
	if len(f) == 0 {
		return "", ""
	}
	op = strings.ToUpper(f[0])
	for i := 0; i < len(f)-1; i++ {
		switch strings.ToUpper(f[i]) {
		case "FROM", "INTO", "TABLE":
			return cleanTable(skipIfNotExists(f, i+1)), op
		}
	}
	if op == "UPDATE" && len(f) > 1 {
		return cleanTable(f[1]), op
	}
	return "", op
}

// skipIfNotExists handles "TABLE IF NOT EXISTS <name>" by jumping past the
// guard tokens to land on the actual table name.
func skipIfNotExists(f []string, i int) string {
	if i+2 < len(f) && strings.EqualFold(f[i], "IF") && strings.EqualFold(f[i+1], "NOT") && strings.EqualFold(f[i+2], "EXISTS") {
		if i+3 < len(f) {
			return f[i+3]
		}
		return ""
	}
	if i+1 < len(f) && strings.EqualFold(f[i], "IF") && strings.EqualFold(f[i+1], "EXISTS") {
		if i+2 < len(f) {
			return f[i+2]
		}
		return ""
	}
	return f[i]
}

func cleanTable(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, ";")
	s = strings.TrimSuffix(s, ",")
	if i := strings.IndexAny(s, "(\n\t "); i > 0 {
		s = s[:i]
	}
	return s
}

// truncateStatement keeps log lines bounded.
func truncateStatement(s string) string {
	const max = 240
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
