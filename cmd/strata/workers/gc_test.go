package workers

import (
	"testing"
	"time"

	"github.com/danchupin/strata/internal/gc"
	datamem "github.com/danchupin/strata/internal/data/memory"
	metamem "github.com/danchupin/strata/internal/meta/memory"
)

func TestGCWorkerRegistered(t *testing.T) {
	w, ok := Lookup("gc")
	if !ok {
		t.Fatal("gc worker not registered (init() did not fire)")
	}
	if w.Name != "gc" {
		t.Fatalf("name=%q want gc", w.Name)
	}
}

func TestBuildGCReadsEnv(t *testing.T) {
	t.Setenv("STRATA_GC_INTERVAL", "7s")
	t.Setenv("STRATA_GC_GRACE", "11m")
	t.Setenv("STRATA_GC_BATCH_SIZE", "250")

	deps := Dependencies{
		Meta:   metamem.New(),
		Data:   datamem.New(),
		Region: "test-region",
	}
	r, err := buildGC(deps)
	if err != nil {
		t.Fatalf("buildGC: %v", err)
	}
	w, ok := r.(*gc.Worker)
	if !ok {
		t.Fatalf("buildGC returned %T, want *gc.Worker", r)
	}
	if w.Interval != 7*time.Second {
		t.Errorf("Interval=%v want 7s", w.Interval)
	}
	if w.Grace != 11*time.Minute {
		t.Errorf("Grace=%v want 11m", w.Grace)
	}
	if w.Batch != 250 {
		t.Errorf("Batch=%d want 250", w.Batch)
	}
	if w.Region != "test-region" {
		t.Errorf("Region=%q", w.Region)
	}
	if w.Meta == nil || w.Data == nil {
		t.Error("Meta/Data not propagated")
	}
}

func TestBuildGCDefaultsWhenEnvUnset(t *testing.T) {
	t.Setenv("STRATA_GC_INTERVAL", "")
	t.Setenv("STRATA_GC_GRACE", "")
	t.Setenv("STRATA_GC_BATCH_SIZE", "")

	r, err := buildGC(Dependencies{Meta: metamem.New(), Data: datamem.New()})
	if err != nil {
		t.Fatalf("buildGC: %v", err)
	}
	w := r.(*gc.Worker)
	if w.Interval != 30*time.Second {
		t.Errorf("Interval=%v want 30s default", w.Interval)
	}
	if w.Grace != 5*time.Minute {
		t.Errorf("Grace=%v want 5m default", w.Grace)
	}
	if w.Batch != 0 {
		t.Errorf("Batch=%d want 0 (gc.Worker default kicks in)", w.Batch)
	}
}
