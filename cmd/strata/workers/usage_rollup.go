package workers

import (
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
	cfg := workerCfg(deps)
	urCfg := cfg.Workers.UsageRollup
	return usagerollup.New(usagerollup.Config{
		Meta:          deps.Meta,
		Logger:        deps.Logger,
		Interval:      orDuration(urCfg.Interval, 24*time.Hour),
		At:            orString(urCfg.At, "00:00"),
		SamplesPerDay: orInt(urCfg.SamplesPerDay, usagerollup.DefaultSamplesPerDay),
		Tracer:        deps.Tracer.Tracer("strata.worker.usage-rollup"),
	})
}
