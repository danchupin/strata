package placement

import (
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/meta"
)

func TestDefaultPolicyNilEmpty(t *testing.T) {
	if got := DefaultPolicy(nil); got != nil {
		t.Fatalf("nil states: want nil, got %v", got)
	}
	if got := DefaultPolicy(map[string]meta.ClusterStateRow{}); got != nil {
		t.Fatalf("empty states: want nil, got %v", got)
	}
}

// DefaultPolicy excludes pending / draining / evacuating / removed and
// any live cluster with weight=0; only state=live + weight>0 contribute.
func TestDefaultPolicyFiltersStates(t *testing.T) {
	states := map[string]meta.ClusterStateRow{
		"live-100":    {State: meta.ClusterStateLive, Weight: 100},
		"live-50":     {State: meta.ClusterStateLive, Weight: 50},
		"live-zero":   {State: meta.ClusterStateLive, Weight: 0},
		"pending":     {State: meta.ClusterStatePending, Weight: 0},
		"pending-w10": {State: meta.ClusterStatePending, Weight: 10},
		"drain-ro":    {State: meta.ClusterStateDrainingReadonly, Mode: meta.ClusterModeReadonly, Weight: 100},
		"evacuating":  {State: meta.ClusterStateEvacuating, Mode: meta.ClusterModeEvacuate, Weight: 25},
		"removed":     {State: meta.ClusterStateRemoved, Weight: 100},
	}
	got := DefaultPolicy(states)
	want := map[string]int{"live-100": 100, "live-50": 50}
	if len(got) != len(want) {
		t.Fatalf("len: want %d, got %d (got=%v)", len(want), len(got), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("key %q: want %d, got %d", k, v, got[k])
		}
	}
}

// DefaultPolicy + PickClusterExcluding integration: 3 live clusters with
// weights {c1:10, c2:30, c3:60} and a nil bucket policy → 1000 PUT keys
// distribute ~100/300/600 within 5% tolerance per AC.
func TestDefaultPolicyDistribution(t *testing.T) {
	states := map[string]meta.ClusterStateRow{
		"c1": {State: meta.ClusterStateLive, Weight: 10},
		"c2": {State: meta.ClusterStateLive, Weight: 30},
		"c3": {State: meta.ClusterStateLive, Weight: 60},
	}
	policy := DefaultPolicy(states)
	bucketID := uuid.New()
	counts := map[string]int{}
	const total = 1000
	for i := range total {
		got := PickCluster(bucketID, fmt.Sprintf("k-%d", i), 0, policy)
		counts[got]++
	}
	want := map[string]float64{"c1": 0.10, "c2": 0.30, "c3": 0.60}
	for cluster, ratio := range want {
		checkRatio(t, counts, cluster, total, ratio, 0.05)
	}
}

// Pending clusters are excluded from synthesised default policy entirely
// — even when their weight field happens to be non-zero (legacy / racey).
func TestDefaultPolicyPendingNeverPicked(t *testing.T) {
	states := map[string]meta.ClusterStateRow{
		"live":    {State: meta.ClusterStateLive, Weight: 50},
		"pending": {State: meta.ClusterStatePending, Weight: 50},
	}
	policy := DefaultPolicy(states)
	bucketID := uuid.New()
	for i := range 1000 {
		got := PickCluster(bucketID, fmt.Sprintf("k-%d", i), 0, policy)
		if got == "pending" {
			t.Fatalf("iter %d: pending cluster picked", i)
		}
	}
}

// All-zero weights + no live cluster → DefaultPolicy returns nil →
// PickCluster returns "" so the caller falls back to per-class
// spec.Cluster (AC: "All live clusters with weight=0 → DefaultPolicy
// returns empty map → PickCluster returns ''").
func TestDefaultPolicyAllZero(t *testing.T) {
	states := map[string]meta.ClusterStateRow{
		"a": {State: meta.ClusterStateLive, Weight: 0},
		"b": {State: meta.ClusterStateLive, Weight: 0},
	}
	policy := DefaultPolicy(states)
	if policy != nil {
		t.Fatalf("all-zero live: want nil, got %v", policy)
	}
	bucketID := uuid.New()
	if got := PickCluster(bucketID, "k", 0, policy); got != "" {
		t.Fatalf("PickCluster on nil policy: want '', got %q", got)
	}
}
