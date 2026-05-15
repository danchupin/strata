package placement

import (
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/meta"
)

// nil bucketPolicy + weighted → return cluster weights synthesised
// default policy (filtered to live + weight>0).
func TestEffectivePolicyNilBucketWeighted(t *testing.T) {
	weights := map[string]int{"y": 50, "z": 50}
	states := map[string]meta.ClusterStateRow{
		"y": {State: meta.ClusterStateLive, Weight: 50},
		"z": {State: meta.ClusterStateLive, Weight: 50},
	}
	got := EffectivePolicy(nil, "weighted", weights, states)
	if len(got) != 2 || got["y"] != 50 || got["z"] != 50 {
		t.Fatalf("nil bucket + weighted: want {y:50,z:50}, got %v", got)
	}
}

// nil bucketPolicy + strict → treated as weighted (strict needs an
// explicit policy to be meaningful); fall back to cluster weights.
func TestEffectivePolicyNilBucketStrict(t *testing.T) {
	weights := map[string]int{"y": 50, "z": 50}
	states := map[string]meta.ClusterStateRow{
		"y": {State: meta.ClusterStateLive, Weight: 50},
		"z": {State: meta.ClusterStateLive, Weight: 50},
	}
	got := EffectivePolicy(nil, "strict", weights, states)
	if len(got) != 2 || got["y"] != 50 || got["z"] != 50 {
		t.Fatalf("nil bucket + strict: want {y:50,z:50}, got %v", got)
	}
}

// mode == "" is treated as weighted (backwards-compat for legacy buckets).
func TestEffectivePolicyEmptyModeIsWeighted(t *testing.T) {
	weights := map[string]int{"y": 50}
	states := map[string]meta.ClusterStateRow{"y": {State: meta.ClusterStateLive, Weight: 50}}
	got := EffectivePolicy(nil, "", weights, states)
	if len(got) != 1 || got["y"] != 50 {
		t.Fatalf("nil bucket + empty mode: want {y:50}, got %v", got)
	}
}

// bucketPolicy all live → return as-is regardless of mode.
func TestEffectivePolicyAllLive(t *testing.T) {
	bucket := map[string]int{"a": 70, "b": 30}
	states := map[string]meta.ClusterStateRow{
		"a": {State: meta.ClusterStateLive, Weight: 100},
		"b": {State: meta.ClusterStateLive, Weight: 100},
	}
	for _, mode := range []string{"", "weighted", "strict"} {
		got := EffectivePolicy(bucket, mode, nil, states)
		if len(got) != 2 || got["a"] != 70 || got["b"] != 30 {
			t.Fatalf("mode=%q all-live: want {a:70,b:30}, got %v", mode, got)
		}
	}
}

// bucketPolicy mixed live/draining → return only the live subset.
func TestEffectivePolicyMixedLiveDraining(t *testing.T) {
	bucket := map[string]int{"a": 70, "b": 30}
	states := map[string]meta.ClusterStateRow{
		"a": {State: meta.ClusterStateLive, Weight: 100},
		"b": {State: meta.ClusterStateEvacuating, Mode: meta.ClusterModeEvacuate, Weight: 100},
	}
	for _, mode := range []string{"", "weighted", "strict"} {
		got := EffectivePolicy(bucket, mode, nil, states)
		if len(got) != 1 || got["a"] != 70 {
			t.Fatalf("mode=%q mixed: want {a:70}, got %v", mode, got)
		}
	}
}

// bucketPolicy all draining + weighted → fallback to cluster weights.
func TestEffectivePolicyAllDrainingWeighted(t *testing.T) {
	bucket := map[string]int{"x": 1}
	weights := map[string]int{"y": 50, "z": 50}
	states := map[string]meta.ClusterStateRow{
		"x": {State: meta.ClusterStateEvacuating, Mode: meta.ClusterModeEvacuate, Weight: 100},
		"y": {State: meta.ClusterStateLive, Weight: 50},
		"z": {State: meta.ClusterStateLive, Weight: 50},
	}
	got := EffectivePolicy(bucket, "weighted", weights, states)
	if len(got) != 2 || got["y"] != 50 || got["z"] != 50 {
		t.Fatalf("all-draining + weighted: want {y:50,z:50}, got %v", got)
	}
}

// bucketPolicy all draining + strict → empty (compliance stickiness).
func TestEffectivePolicyAllDrainingStrict(t *testing.T) {
	bucket := map[string]int{"x": 1}
	weights := map[string]int{"y": 50, "z": 50}
	states := map[string]meta.ClusterStateRow{
		"x": {State: meta.ClusterStateEvacuating, Mode: meta.ClusterModeEvacuate, Weight: 100},
		"y": {State: meta.ClusterStateLive, Weight: 50},
		"z": {State: meta.ClusterStateLive, Weight: 50},
	}
	got := EffectivePolicy(bucket, "strict", weights, states)
	if got != nil {
		t.Fatalf("all-draining + strict: want nil, got %v", got)
	}
}

// All clusters drained both ways → empty regardless of mode.
func TestEffectivePolicyAllClustersDrained(t *testing.T) {
	bucket := map[string]int{"x": 1, "y": 1}
	weights := map[string]int{"x": 50, "y": 50}
	states := map[string]meta.ClusterStateRow{
		"x": {State: meta.ClusterStateEvacuating, Mode: meta.ClusterModeEvacuate, Weight: 50},
		"y": {State: meta.ClusterStateDrainingReadonly, Mode: meta.ClusterModeReadonly, Weight: 50},
	}
	for _, mode := range []string{"", "weighted", "strict"} {
		got := EffectivePolicy(bucket, mode, weights, states)
		if got != nil {
			t.Fatalf("mode=%q all-drained: want nil, got %v", mode, got)
		}
	}
}

// Pending clusters in bucket policy are excluded (treated like draining
// — anything non-live drops out of the live subset).
func TestEffectivePolicyPendingExcluded(t *testing.T) {
	bucket := map[string]int{"p": 1}
	weights := map[string]int{"y": 100}
	states := map[string]meta.ClusterStateRow{
		"p": {State: meta.ClusterStatePending, Weight: 0},
		"y": {State: meta.ClusterStateLive, Weight: 100},
	}
	got := EffectivePolicy(bucket, "weighted", weights, states)
	if len(got) != 1 || got["y"] != 100 {
		t.Fatalf("pending excluded: want {y:100}, got %v", got)
	}
	gotStrict := EffectivePolicy(bucket, "strict", weights, states)
	if gotStrict != nil {
		t.Fatalf("pending excluded + strict: want nil, got %v", gotStrict)
	}
}

// Removed clusters in bucket policy are excluded.
func TestEffectivePolicyRemovedExcluded(t *testing.T) {
	bucket := map[string]int{"r": 1, "y": 1}
	states := map[string]meta.ClusterStateRow{
		"r": {State: meta.ClusterStateRemoved},
		"y": {State: meta.ClusterStateLive, Weight: 100},
	}
	got := EffectivePolicy(bucket, "weighted", nil, states)
	if len(got) != 1 || got["y"] != 1 {
		t.Fatalf("removed excluded: want {y:1}, got %v", got)
	}
}

// Absent cluster_state row → treated as live (per cluster_state semantic
// "absence == live").
func TestEffectivePolicyAbsentRowIsLive(t *testing.T) {
	bucket := map[string]int{"x": 1}
	// states map intentionally empty — no row for x.
	states := map[string]meta.ClusterStateRow{}
	got := EffectivePolicy(bucket, "weighted", nil, states)
	if len(got) != 1 || got["x"] != 1 {
		t.Fatalf("absent row treated as live: want {x:1}, got %v", got)
	}
}

// Distribution: bucket Placement={X:1}, X draining, mode=weighted,
// cluster.weights={Y:50,Z:50} → 1000 PUTs split ~50/50 on Y/Z (5% tol).
func TestEffectivePolicyDistributionWeightedFallback(t *testing.T) {
	bucket := map[string]int{"x": 1}
	weights := map[string]int{"y": 50, "z": 50}
	states := map[string]meta.ClusterStateRow{
		"x": {State: meta.ClusterStateEvacuating, Mode: meta.ClusterModeEvacuate, Weight: 100},
		"y": {State: meta.ClusterStateLive, Weight: 50},
		"z": {State: meta.ClusterStateLive, Weight: 50},
	}
	policy := EffectivePolicy(bucket, "weighted", weights, states)
	if len(policy) != 2 {
		t.Fatalf("expected fallback policy {y,z}, got %v", policy)
	}
	bucketID := uuid.New()
	counts := map[string]int{}
	const total = 1000
	for i := range total {
		got := PickCluster(bucketID, fmt.Sprintf("k-%d", i), 0, policy)
		counts[got]++
		if got == "x" {
			t.Fatalf("iter %d: draining cluster x picked", i)
		}
	}
	checkRatio(t, counts, "y", total, 0.5, 0.05)
	checkRatio(t, counts, "z", total, 0.5, 0.05)
}

// Distribution: same setup but mode=strict → policy empty → all 1000
// PickCluster calls return "" → caller observes 503 / strict-refuse path.
func TestEffectivePolicyDistributionStrictRefuse(t *testing.T) {
	bucket := map[string]int{"x": 1}
	weights := map[string]int{"y": 50, "z": 50}
	states := map[string]meta.ClusterStateRow{
		"x": {State: meta.ClusterStateEvacuating, Mode: meta.ClusterModeEvacuate, Weight: 100},
		"y": {State: meta.ClusterStateLive, Weight: 50},
		"z": {State: meta.ClusterStateLive, Weight: 50},
	}
	policy := EffectivePolicy(bucket, "strict", weights, states)
	if policy != nil {
		t.Fatalf("expected nil policy for strict + all-draining, got %v", policy)
	}
	bucketID := uuid.New()
	for i := range 1000 {
		got := PickCluster(bucketID, fmt.Sprintf("k-%d", i), 0, policy)
		if got != "" {
			t.Fatalf("iter %d: strict-refused policy returned non-empty %q", i, got)
		}
	}
}

// Empty cluster weights + all-draining bucket policy + weighted → both
// empty → return nil (genuine no-target).
func TestEffectivePolicyBothEmpty(t *testing.T) {
	bucket := map[string]int{"x": 1}
	states := map[string]meta.ClusterStateRow{
		"x": {State: meta.ClusterStateEvacuating, Mode: meta.ClusterModeEvacuate, Weight: 100},
	}
	got := EffectivePolicy(bucket, "weighted", nil, states)
	if got != nil {
		t.Fatalf("both empty: want nil, got %v", got)
	}
}

// Zero-weight entry in bucket policy is filtered out (matches
// PickClusterExcluding semantic — never picked).
func TestEffectivePolicyZeroWeightFiltered(t *testing.T) {
	bucket := map[string]int{"a": 0, "b": 50}
	states := map[string]meta.ClusterStateRow{
		"a": {State: meta.ClusterStateLive, Weight: 100},
		"b": {State: meta.ClusterStateLive, Weight: 100},
	}
	got := EffectivePolicy(bucket, "weighted", nil, states)
	if len(got) != 1 || got["b"] != 50 {
		t.Fatalf("zero-weight filtered: want {b:50}, got %v", got)
	}
}
