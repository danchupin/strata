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
	bucket := strings.TrimSpace(stringFromEnv("STRATA_AUDIT_EXPORT_BUCKET", ""))
	if bucket == "" {
		return nil, errors.New("audit-export: STRATA_AUDIT_EXPORT_BUCKET is required")
	}
	return auditexport.New(auditexport.Config{
		Meta:     deps.Meta,
		Data:     deps.Data,
		Logger:   deps.Logger,
		Bucket:   bucket,
		Prefix:   stringFromEnv("STRATA_AUDIT_EXPORT_PREFIX", ""),
		After:    durationFromEnv("STRATA_AUDIT_EXPORT_AFTER", 30*24*time.Hour),
		Interval: durationFromEnv("STRATA_AUDIT_EXPORT_INTERVAL", 24*time.Hour),
		Tracer:   deps.Tracer.Tracer("strata.worker.audit-export"),
	})
}
