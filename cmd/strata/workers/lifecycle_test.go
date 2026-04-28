package workers

import (
	"testing"
	"time"

	datamem "github.com/danchupin/strata/internal/data/memory"
	"github.com/danchupin/strata/internal/lifecycle"
	metamem "github.com/danchupin/strata/internal/meta/memory"
)

func TestLifecycleWorkerRegistered(t *testing.T) {
	w, ok := Lookup("lifecycle")
	if !ok {
		t.Fatal("lifecycle worker not registered (init() did not fire)")
	}
	if w.Name != "lifecycle" {
		t.Fatalf("name=%q want lifecycle", w.Name)
	}
}

func TestBuildLifecycleReadsEnv(t *testing.T) {
	t.Setenv("STRATA_LIFECYCLE_INTERVAL", "9s")
	t.Setenv("STRATA_LIFECYCLE_UNIT", "hour")

	deps := Dependencies{
		Meta:   metamem.New(),
		Data:   datamem.New(),
		Region: "test-region",
	}
	r, err := buildLifecycle(deps)
	if err != nil {
		t.Fatalf("buildLifecycle: %v", err)
	}
	w, ok := r.(*lifecycle.Worker)
	if !ok {
		t.Fatalf("buildLifecycle returned %T, want *lifecycle.Worker", r)
	}
	if w.Interval != 9*time.Second {
		t.Errorf("Interval=%v want 9s", w.Interval)
	}
	if w.AgeUnit != time.Hour {
		t.Errorf("AgeUnit=%v want 1h", w.AgeUnit)
	}
	if w.Region != "test-region" {
		t.Errorf("Region=%q", w.Region)
	}
	if w.Meta == nil || w.Data == nil {
		t.Error("Meta/Data not propagated")
	}
}

func TestBuildLifecycleDefaultsWhenEnvUnset(t *testing.T) {
	t.Setenv("STRATA_LIFECYCLE_INTERVAL", "")
	t.Setenv("STRATA_LIFECYCLE_UNIT", "")

	r, err := buildLifecycle(Dependencies{Meta: metamem.New(), Data: datamem.New()})
	if err != nil {
		t.Fatalf("buildLifecycle: %v", err)
	}
	w := r.(*lifecycle.Worker)
	if w.Interval != 60*time.Second {
		t.Errorf("Interval=%v want 60s default", w.Interval)
	}
	if w.AgeUnit != 24*time.Hour {
		t.Errorf("AgeUnit=%v want 24h default", w.AgeUnit)
	}
}

func TestAgeUnitFromEnv(t *testing.T) {
	cases := map[string]time.Duration{
		"second": time.Second,
		"minute": time.Minute,
		"hour":   time.Hour,
		"day":    24 * time.Hour,
		"":       7 * time.Hour, // fallback
		"weird":  7 * time.Hour, // fallback
	}
	for in, want := range cases {
		t.Setenv("STRATA_LIFECYCLE_UNIT", in)
		got := ageUnitFromEnv("STRATA_LIFECYCLE_UNIT", 7*time.Hour)
		if got != want {
			t.Errorf("ageUnitFromEnv(%q)=%v want %v", in, got, want)
		}
	}
}
