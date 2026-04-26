package rados

import (
	"context"
	"log/slog"
	"time"

	"github.com/danchupin/strata/internal/logging"
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
