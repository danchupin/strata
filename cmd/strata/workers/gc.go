package workers

import (
	"os"
	"strconv"
	"time"

	"github.com/danchupin/strata/internal/gc"
	"github.com/danchupin/strata/internal/metrics"
)

func init() {
	Register(Worker{
		Name:  "gc",
		Build: buildGC,
	})
}

func buildGC(deps Dependencies) (Runner, error) {
	return &gc.Worker{
		Meta:        deps.Meta,
		Data:        deps.Data,
		Region:      deps.Region,
		Interval:    durationFromEnv("STRATA_GC_INTERVAL", 30*time.Second),
		Grace:       durationFromEnv("STRATA_GC_GRACE", 5*time.Minute),
		Batch:       intFromEnv("STRATA_GC_BATCH_SIZE", 0),
		Concurrency: clampConcurrency(intFromEnv("STRATA_GC_CONCURRENCY", 1)),
		Logger:      deps.Logger,
		Metrics:     metrics.GCObserver{},
	}, nil
}

// clampConcurrency clamps to [1, 256]; zero/negative -> 1.
func clampConcurrency(n int) int {
	if n < 1 {
		return 1
	}
	if n > 256 {
		return 256
	}
	return n
}

func durationFromEnv(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}

func intFromEnv(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}
