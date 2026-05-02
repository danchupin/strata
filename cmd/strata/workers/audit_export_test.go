package workers

import (
	"strings"
	"testing"
	"time"

	"github.com/danchupin/strata/internal/auditexport"
	datamem "github.com/danchupin/strata/internal/data/memory"
	metamem "github.com/danchupin/strata/internal/meta/memory"
)

func TestAuditExportWorkerRegistered(t *testing.T) {
	w, ok := Lookup("audit-export")
	if !ok {
		t.Fatal("audit-export worker not registered (init() did not fire)")
	}
	if w.Name != "audit-export" {
		t.Fatalf("name=%q want audit-export", w.Name)
	}
}

func TestBuildAuditExportReadsEnv(t *testing.T) {
	t.Setenv("STRATA_AUDIT_EXPORT_BUCKET", "audit-archive")
	t.Setenv("STRATA_AUDIT_EXPORT_PREFIX", "logs/")
	t.Setenv("STRATA_AUDIT_EXPORT_AFTER", "240h")
	t.Setenv("STRATA_AUDIT_EXPORT_INTERVAL", "1h")

	r, err := buildAuditExport(Dependencies{Meta: metamem.New(), Data: datamem.New()})
	if err != nil {
		t.Fatalf("buildAuditExport: %v", err)
	}
	if _, ok := r.(*auditexport.Worker); !ok {
		t.Fatalf("buildAuditExport returned %T, want *auditexport.Worker", r)
	}
}

func TestBuildAuditExportRequiresBucket(t *testing.T) {
	t.Setenv("STRATA_AUDIT_EXPORT_BUCKET", "")

	_, err := buildAuditExport(Dependencies{Meta: metamem.New(), Data: datamem.New()})
	if err == nil {
		t.Fatal("buildAuditExport: want error for missing bucket, got nil")
	}
	if !strings.Contains(err.Error(), "STRATA_AUDIT_EXPORT_BUCKET") {
		t.Errorf("error = %q, want mention of STRATA_AUDIT_EXPORT_BUCKET", err.Error())
	}
}

func TestBuildAuditExportDefaultsWhenEnvUnset(t *testing.T) {
	t.Setenv("STRATA_AUDIT_EXPORT_BUCKET", "audit-archive")
	t.Setenv("STRATA_AUDIT_EXPORT_PREFIX", "")
	t.Setenv("STRATA_AUDIT_EXPORT_AFTER", "")
	t.Setenv("STRATA_AUDIT_EXPORT_INTERVAL", "")

	r, err := buildAuditExport(Dependencies{Meta: metamem.New(), Data: datamem.New()})
	if err != nil {
		t.Fatalf("buildAuditExport: %v", err)
	}
	if _, ok := r.(*auditexport.Worker); !ok {
		t.Fatalf("buildAuditExport returned %T, want *auditexport.Worker", r)
	}
}

func TestAuditExportDefaultsMatchLegacy(t *testing.T) {
	if got := durationFromEnv("STRATA_AUDIT_EXPORT_AFTER_UNSET", 30*24*time.Hour); got != 30*24*time.Hour {
		t.Errorf("after default = %v, want %v", got, 30*24*time.Hour)
	}
	if got := durationFromEnv("STRATA_AUDIT_EXPORT_INTERVAL_UNSET", 24*time.Hour); got != 24*time.Hour {
		t.Errorf("interval default = %v, want %v", got, 24*time.Hour)
	}
}
