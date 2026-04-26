package cassandra

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gocql/gocql"

	"github.com/danchupin/strata/internal/logging"
)

// EnvSlowQueryMS controls the WARN threshold for SlowQueryObserver. Default 100ms.
const EnvSlowQueryMS = "STRATA_CASSANDRA_SLOW_MS"

// DefaultSlowQueryMS is the default WARN threshold when STRATA_CASSANDRA_SLOW_MS is unset.
const DefaultSlowQueryMS = 100

// SlowQueryObserver implements gocql.QueryObserver. Queries that exceed
// Threshold (or fail with an error) are logged at WARN with structured
// attributes including request_id, table, op, duration_ms, statement.
type SlowQueryObserver struct {
	Logger    *slog.Logger
	Threshold time.Duration
}

// NewSlowQueryObserver returns nil when threshold <= 0 (disabled) or logger is nil.
func NewSlowQueryObserver(logger *slog.Logger, threshold time.Duration) *SlowQueryObserver {
	if logger == nil || threshold <= 0 {
		return nil
	}
	return &SlowQueryObserver{Logger: logger, Threshold: threshold}
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

// ObserveQuery is called by gocql for every query. Logs only when the elapsed
// time meets the threshold or the query failed.
func (o *SlowQueryObserver) ObserveQuery(ctx context.Context, q gocql.ObservedQuery) {
	dur := q.End.Sub(q.Start)
	if q.Err == nil && dur < o.Threshold {
		return
	}
	table, op := parseStatement(q.Statement)
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
