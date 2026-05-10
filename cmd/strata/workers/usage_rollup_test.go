package workers

import (
	"testing"
	"time"

	datamem "github.com/danchupin/strata/internal/data/memory"
	metamem "github.com/danchupin/strata/internal/meta/memory"
)

func TestUsageRollupWorkerRegistered(t *testing.T) {
	w, ok := Lookup("usage-rollup")
	if !ok {
		t.Fatal("usage-rollup worker not registered (init() did not fire)")
	}
	if w.Name != "usage-rollup" {
		t.Fatalf("name=%q want usage-rollup", w.Name)
	}
	if w.SkipLease {
		t.Errorf("SkipLease=true; usage-rollup expects supervisor-managed lease")
	}
}

func TestBuildUsageRollupReadsEnv(t *testing.T) {
	t.Setenv("STRATA_USAGE_ROLLUP_INTERVAL", "12h")
	t.Setenv("STRATA_USAGE_ROLLUP_AT", "06:30")
	r, err := buildUsageRollup(Dependencies{Meta: metamem.New(), Data: datamem.New()})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if r == nil {
		t.Fatal("nil runner")
	}
}

func TestBuildUsageRollupDefaultsWhenEnvUnset(t *testing.T) {
	t.Setenv("STRATA_USAGE_ROLLUP_INTERVAL", "")
	t.Setenv("STRATA_USAGE_ROLLUP_AT", "")
	r, err := buildUsageRollup(Dependencies{Meta: metamem.New(), Data: datamem.New()})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if r == nil {
		t.Fatal("nil runner")
	}
}

func TestBuildUsageRollupRequiresMeta(t *testing.T) {
	if _, err := buildUsageRollup(Dependencies{}); err == nil {
		t.Fatal("want error for missing meta")
	}
}

func TestBuildUsageRollupRejectsBadAt(t *testing.T) {
	t.Setenv("STRATA_USAGE_ROLLUP_AT", "not-a-time")
	if _, err := buildUsageRollup(Dependencies{Meta: metamem.New()}); err == nil {
		t.Fatal("want error for invalid STRATA_USAGE_ROLLUP_AT")
	}
}

func TestUsageRollupResolvesViaRegistry(t *testing.T) {
	got, err := Resolve([]string{"usage-rollup"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got) != 1 || got[0].Name != "usage-rollup" {
		t.Fatalf("Resolve returned %+v", got)
	}
	_ = time.Second
}
