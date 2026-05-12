package placement

import (
	"fmt"
	"math"
	"testing"

	"github.com/google/uuid"
)

func TestPickClusterEmptyPolicy(t *testing.T) {
	bucketID := uuid.New()
	if got := PickCluster(bucketID, "key", 0, nil); got != "" {
		t.Fatalf("nil policy: want %q, got %q", "", got)
	}
	if got := PickCluster(bucketID, "key", 0, map[string]int{}); got != "" {
		t.Fatalf("empty policy: want %q, got %q", "", got)
	}
}

func TestPickClusterAllZeroWeights(t *testing.T) {
	bucketID := uuid.New()
	policy := map[string]int{"a": 0, "b": 0}
	if got := PickCluster(bucketID, "key", 0, policy); got != "" {
		t.Fatalf("all-zero policy: want %q, got %q", "", got)
	}
}

// Zero-weight clusters in the policy are never picked.
func TestPickClusterSkipsZeroWeights(t *testing.T) {
	bucketID := uuid.New()
	policy := map[string]int{"a": 0, "b": 1, "c": 0}
	for i := range 1000 {
		got := PickCluster(bucketID, fmt.Sprintf("k-%d", i), i, policy)
		if got != "b" {
			t.Fatalf("iter %d: zero-weight cluster picked: got %q (only %q has weight)", i, got, "b")
		}
	}
}

// Determinism: identical inputs produce identical outputs across calls.
func TestPickClusterDeterministic(t *testing.T) {
	bucketID := uuid.New()
	policy := map[string]int{"a": 1, "b": 1, "c": 1}
	for i := range 1000 {
		key := fmt.Sprintf("obj-%d", i)
		first := PickCluster(bucketID, key, i%7, policy)
		for r := range 4 {
			got := PickCluster(bucketID, key, i%7, policy)
			if got != first {
				t.Fatalf("iter %d/%d: non-deterministic — first=%q got=%q", i, r, first, got)
			}
		}
	}
}

// Distribution: weights {a:1, b:1} split ~50/50 (within 5%).
func TestPickClusterDistributionEqual(t *testing.T) {
	bucketID := uuid.New()
	policy := map[string]int{"a": 1, "b": 1}
	const total = 10000
	counts := map[string]int{}
	for i := range total {
		counts[PickCluster(bucketID, fmt.Sprintf("k-%d", i), 0, policy)]++
	}
	checkRatio(t, counts, "a", total, 0.5, 0.05)
	checkRatio(t, counts, "b", total, 0.5, 0.05)
}

// Distribution: weights {a:1, b:3} split ~25/75 (within 5%).
func TestPickClusterDistributionWeighted(t *testing.T) {
	bucketID := uuid.New()
	policy := map[string]int{"a": 1, "b": 3}
	const total = 10000
	counts := map[string]int{}
	for i := range total {
		counts[PickCluster(bucketID, fmt.Sprintf("k-%d", i), 0, policy)]++
	}
	checkRatio(t, counts, "a", total, 0.25, 0.05)
	checkRatio(t, counts, "b", total, 0.75, 0.05)
}

// Sorted-cluster order: independent of Go map iteration order.
// Build the same logical policy two different ways and assert identical
// outputs over 1000 keys.
func TestPickClusterSortedOrder(t *testing.T) {
	bucketID := uuid.New()
	a := map[string]int{"a": 1, "b": 1, "c": 1}
	b := map[string]int{"c": 1, "b": 1, "a": 1}
	for i := range 1000 {
		key := fmt.Sprintf("k-%d", i)
		ga := PickCluster(bucketID, key, 0, a)
		gb := PickCluster(bucketID, key, 0, b)
		if ga != gb {
			t.Fatalf("iter %d: map-order dependence ga=%q gb=%q", i, ga, gb)
		}
	}
}

// PickClusterExcluding: entries in `excluded` are treated as weight=0.
func TestPickClusterExcluding(t *testing.T) {
	bucketID := uuid.New()
	policy := map[string]int{"a": 1, "b": 1, "c": 1}
	excluded := map[string]bool{"b": true}
	for i := range 1000 {
		got := PickClusterExcluding(bucketID, fmt.Sprintf("k-%d", i), i, policy, excluded)
		if got == "b" {
			t.Fatalf("iter %d: excluded cluster picked: %q", i, got)
		}
	}
}

// PickClusterExcluding: all-excluded returns "" so caller falls back.
func TestPickClusterExcludingAll(t *testing.T) {
	bucketID := uuid.New()
	policy := map[string]int{"a": 1, "b": 1}
	excluded := map[string]bool{"a": true, "b": true}
	if got := PickClusterExcluding(bucketID, "key", 0, policy, excluded); got != "" {
		t.Fatalf("all-excluded: want %q, got %q", "", got)
	}
}

func checkRatio(t *testing.T, counts map[string]int, cluster string, total int, want, tolerance float64) {
	t.Helper()
	got := float64(counts[cluster]) / float64(total)
	if math.Abs(got-want) > tolerance {
		t.Fatalf("cluster %q ratio: want %.3f ± %.3f, got %.3f (%d/%d)",
			cluster, want, tolerance, got, counts[cluster], total)
	}
}
