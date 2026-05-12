package memory

import (
	"context"
	"testing"
)

// TestDataHealthClusterEmpty pins the single virtual-cluster contract:
// the memory backend's synthetic pool row carries Cluster="" so the UI
// renders "—" instead of crashing on null. Guards US-001 (placement-ui)
// against a future refactor that defaults the field to "default".
func TestDataHealthClusterEmpty(t *testing.T) {
	b := New()
	report, err := b.DataHealth(context.Background())
	if err != nil {
		t.Fatalf("DataHealth: %v", err)
	}
	if report == nil || len(report.Pools) != 1 {
		t.Fatalf("want 1 pool row, got %+v", report)
	}
	if report.Pools[0].Cluster != "" {
		t.Errorf("Cluster=%q want empty", report.Pools[0].Cluster)
	}
}
