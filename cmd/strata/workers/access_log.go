package workers

import (
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
	cfg := workerCfg(deps)
	alCfg := cfg.Workers.AccessLog
	return accesslog.New(accesslog.Config{
		Meta:          deps.Meta,
		Data:          deps.Data,
		Logger:        deps.Logger,
		Interval:      orDuration(alCfg.Interval, 5*time.Minute),
		MaxFlushBytes: orInt64(alCfg.MaxFlushBytes, 5*1024*1024),
		PollLimit:     orInt(alCfg.PollLimit, 10000),
		Tracer:        deps.Tracer.Tracer("strata.worker.access-log"),
	})
}
