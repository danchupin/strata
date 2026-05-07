package workers

import (
	"os"
	"time"

	"github.com/danchupin/strata/internal/lifecycle"
)

func init() {
	Register(Worker{
		Name:  "lifecycle",
		Build: buildLifecycle,
	})
}

func buildLifecycle(deps Dependencies) (Runner, error) {
	return &lifecycle.Worker{
		Meta:        deps.Meta,
		Data:        deps.Data,
		Region:      deps.Region,
		Interval:    durationFromEnv("STRATA_LIFECYCLE_INTERVAL", 60*time.Second),
		AgeUnit:     ageUnitFromEnv("STRATA_LIFECYCLE_UNIT", 24*time.Hour),
		Concurrency: clampConcurrency(intFromEnv("STRATA_LIFECYCLE_CONCURRENCY", 1)),
		Logger:      deps.Logger,
	}, nil
}

// ageUnitFromEnv mirrors the legacy cmd/strata-lifecycle string -> duration
// switch so existing STRATA_LIFECYCLE_UNIT values (second/minute/hour/day)
// keep working byte-for-byte.
func ageUnitFromEnv(key string, fallback time.Duration) time.Duration {
	switch os.Getenv(key) {
	case "second":
		return time.Second
	case "minute":
		return time.Minute
	case "hour":
		return time.Hour
	case "day":
		return 24 * time.Hour
	}
	return fallback
}
