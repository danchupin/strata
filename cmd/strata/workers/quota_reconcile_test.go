package workers

import (
	"testing"
	"time"

	datamem "github.com/danchupin/strata/internal/data/memory"
	metamem "github.com/danchupin/strata/internal/meta/memory"
)

func TestQuotaReconcileWorkerRegistered(t *testing.T) {
	w, ok := Lookup("quota-reconcile")
	if !ok {
		t.Fatal("quota-reconcile worker not registered (init() did not fire)")
	}
	if w.Name != "quota-reconcile" {
		t.Fatalf("name=%q want quota-reconcile", w.Name)
	}
	if w.SkipLease {
		t.Errorf("SkipLease=true; quota-reconcile expects supervisor-managed lease")
	}
}

func TestBuildQuotaReconcileReadsEnv(t *testing.T) {
	t.Setenv("STRATA_QUOTA_RECONCILE_INTERVAL", "30m")
	r, err := buildQuotaReconcile(Dependencies{Meta: metamem.New(), Data: datamem.New()})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if r == nil {
		t.Fatal("nil runner")
	}
}

func TestBuildQuotaReconcileDefaultsWhenEnvUnset(t *testing.T) {
	t.Setenv("STRATA_QUOTA_RECONCILE_INTERVAL", "")
	r, err := buildQuotaReconcile(Dependencies{Meta: metamem.New(), Data: datamem.New()})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if r == nil {
		t.Fatal("nil runner")
	}
}

func TestBuildQuotaReconcileRequiresMeta(t *testing.T) {
	if _, err := buildQuotaReconcile(Dependencies{}); err == nil {
		t.Fatal("want error for missing meta")
	}
}

func TestQuotaReconcileResolvesViaRegistry(t *testing.T) {
	got, err := Resolve([]string{"quota-reconcile"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got) != 1 || got[0].Name != "quota-reconcile" {
		t.Fatalf("Resolve returned %+v", got)
	}
	_ = time.Second // anchor time import
}
