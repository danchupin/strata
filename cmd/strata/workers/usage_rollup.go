package workers

import (
	"os"
	"time"

	"github.com/danchupin/strata/internal/usagerollup"
)

func init() {
	Register(Worker{
		Name:  "usage-rollup",
		Build: buildUsageRollup,
	})
}

func buildUsageRollup(deps Dependencies) (Runner, error) {
	at := os.Getenv("STRATA_USAGE_ROLLUP_AT")
	if at == "" {
		at = "00:00"
	}
	return usagerollup.New(usagerollup.Config{
		Meta:     deps.Meta,
		Logger:   deps.Logger,
		Interval: durationFromEnv("STRATA_USAGE_ROLLUP_INTERVAL", 24*time.Hour),
		At:       at,
		Tracer:   deps.Tracer.Tracer("strata.worker.usage-rollup"),
	})
}
