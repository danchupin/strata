package workers

import (
	"time"

	"github.com/danchupin/strata/internal/quotareconcile"
)

func init() {
	Register(Worker{
		Name:  "quota-reconcile",
		Build: buildQuotaReconcile,
	})
}

func buildQuotaReconcile(deps Dependencies) (Runner, error) {
	cfg := workerCfg(deps)
	return quotareconcile.New(quotareconcile.Config{
		Meta:     deps.Meta,
		Logger:   deps.Logger,
		Interval: orDuration(cfg.Workers.QuotaReconcile.Interval, 6*time.Hour),
		Tracer:   deps.Tracer.Tracer("strata.worker.quota-reconcile"),
	})
}
