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
	return quotareconcile.New(quotareconcile.Config{
		Meta:     deps.Meta,
		Logger:   deps.Logger,
		Interval: durationFromEnv("STRATA_QUOTA_RECONCILE_INTERVAL", 6*time.Hour),
		Tracer:   deps.Tracer.Tracer("strata.worker.quota-reconcile"),
	})
}
