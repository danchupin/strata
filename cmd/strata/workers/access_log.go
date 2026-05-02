package workers

import (
	"os"
	"strconv"
	"time"

	"github.com/danchupin/strata/internal/accesslog"
)

func init() {
	Register(Worker{
		Name:  "access-log",
		Build: buildAccessLog,
	})
}

func buildAccessLog(deps Dependencies) (Runner, error) {
	return accesslog.New(accesslog.Config{
		Meta:          deps.Meta,
		Data:          deps.Data,
		Logger:        deps.Logger,
		Interval:      durationFromEnv("STRATA_ACCESS_LOG_INTERVAL", 5*time.Minute),
		MaxFlushBytes: int64FromEnv("STRATA_ACCESS_LOG_MAX_FLUSH_BYTES", 5*1024*1024),
		PollLimit:     intFromEnv("STRATA_ACCESS_LOG_POLL_LIMIT", 10000),
	})
}

func int64FromEnv(key string, fallback int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return fallback
}
