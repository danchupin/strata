package workers

import (
	"errors"
	"strings"
	"time"

	"github.com/danchupin/strata/internal/auditexport"
)

func init() {
	Register(Worker{
		Name:  "audit-export",
		Build: buildAuditExport,
	})
}

func buildAuditExport(deps Dependencies) (Runner, error) {
	cfg := workerCfg(deps)
	aeCfg := cfg.Workers.AuditExport
	bucket := strings.TrimSpace(aeCfg.Bucket)
	if bucket == "" {
		return nil, errors.New("audit-export: workers.audit_export.bucket (STRATA_AUDIT_EXPORT_BUCKET) is required")
	}
	return auditexport.New(auditexport.Config{
		Meta:     deps.Meta,
		Data:     deps.Data,
		Logger:   deps.Logger,
		Bucket:   bucket,
		Prefix:   aeCfg.Prefix,
		After:    orDuration(aeCfg.After, 30*24*time.Hour),
		Interval: orDuration(aeCfg.Interval, 24*time.Hour),
		Tracer:   deps.Tracer.Tracer("strata.worker.audit-export"),
	})
}
