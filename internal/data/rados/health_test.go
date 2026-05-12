package rados

import (
	"sort"
	"testing"
)

// TestBuildPendingPoolStatusesCrossProduct asserts that the helper emits
// one row per (cluster, distinct-pool) cell across the cross-product —
// the Pools table reflects actual per-cluster distribution, not just
// the class env routing config. Lab shape: 2 clusters × 3 distinct
// pools → 6 rows. Guards US-001 of drain-lifecycle.
func TestBuildPendingPoolStatusesCrossProduct(t *testing.T) {
	classes := map[string]ClassSpec{
		"STANDARD": {Cluster: "default", Pool: "hot"},
		"IA":       {Cluster: "default", Pool: "warm"},
		"GLACIER":  {Cluster: "default", Pool: "cold"},
	}
	clusters := map[string]ClusterSpec{
		"default": {ID: "default"},
		"cephb":   {ID: "cephb"},
	}
	got := buildPendingPoolStatuses(classes, clusters)
	if len(got) != 6 {
		t.Fatalf("want 6 rows (2 clusters × 3 pools), got %d (%+v)", len(got), got)
	}
	byCluster := map[string]int{}
	for _, p := range got {
		byCluster[p.status.Cluster]++
		if p.status.Cluster == "" {
			t.Errorf("row %s has empty Cluster", p.status.Name)
		}
	}
	if byCluster["default"] != 3 || byCluster["cephb"] != 3 {
		t.Errorf("per-cluster count mismatch: %v want default=3 cephb=3", byCluster)
	}
}

// TestBuildPendingPoolStatusesTwoClusters keeps the older two-cluster-
// shape guard from placement-ui US-001: distinct clusters surface
// distinct Cluster values on the rows.
func TestBuildPendingPoolStatusesTwoClusters(t *testing.T) {
	classes := map[string]ClassSpec{
		"STANDARD": {Cluster: "eu", Pool: "hot.eu"},
		"COLD":     {Cluster: "us", Pool: "cold.us"},
	}
	clusters := map[string]ClusterSpec{
		"eu": {ID: "eu"},
		"us": {ID: "us"},
	}
	got := buildPendingPoolStatuses(classes, clusters)
	// 2 clusters × 2 distinct pools = 4 rows.
	if len(got) != 4 {
		t.Fatalf("want 4 rows, got %d (%+v)", len(got), got)
	}
	seen := map[string]struct{}{}
	for _, p := range got {
		if p.status.Cluster == "" {
			t.Errorf("row %s has empty Cluster", p.status.Name)
		}
		seen[p.status.Cluster] = struct{}{}
	}
	if len(seen) < 2 {
		t.Fatalf("want >= 2 distinct Cluster values, got %d (%v)", len(seen), seen)
	}
}

// TestBuildPendingPoolStatusesEmptyClusterFallsBackToDefault pins the
// substitution from "" to DefaultCluster when the clusters map carries
// an "" id — config without an explicit cluster still produces a
// populated Cluster field so the UI never renders "no cluster".
func TestBuildPendingPoolStatusesEmptyClusterFallsBackToDefault(t *testing.T) {
	classes := map[string]ClassSpec{
		"STANDARD": {Cluster: "", Pool: "hot.pool"},
	}
	clusters := map[string]ClusterSpec{
		"": {},
	}
	got := buildPendingPoolStatuses(classes, clusters)
	if len(got) != 1 {
		t.Fatalf("want 1 row, got %d", len(got))
	}
	if got[0].status.Cluster != DefaultCluster {
		t.Errorf("Cluster=%q want %q", got[0].status.Cluster, DefaultCluster)
	}
}

// TestBuildPendingPoolStatusesGroupsByPool asserts that two classes
// pointing at the same (pool, ns) collapse the Class field on every
// (cluster, pool) row into the sorted-comma-joined list of classes —
// regardless of how many clusters share the matrix.
func TestBuildPendingPoolStatusesGroupsByPool(t *testing.T) {
	classes := map[string]ClassSpec{
		"STANDARD":    {Cluster: "default", Pool: "hot.pool"},
		"STANDARD_IA": {Cluster: "default", Pool: "hot.pool"},
	}
	clusters := map[string]ClusterSpec{
		"default": {ID: "default"},
		"cephb":   {ID: "cephb"},
	}
	got := buildPendingPoolStatuses(classes, clusters)
	// 2 clusters × 1 distinct pool = 2 rows, both labelled with the
	// joined class list.
	if len(got) != 2 {
		t.Fatalf("want 2 rows, got %d (%+v)", len(got), got)
	}
	for _, p := range got {
		if p.status.Class != "STANDARD,STANDARD_IA" {
			t.Errorf("cluster=%s Class=%q want STANDARD,STANDARD_IA", p.status.Cluster, p.status.Class)
		}
	}
}

// TestBuildPendingPoolStatusesSortOrder pins (Cluster, Name) ascending
// sort so the wire output is deterministic across renders.
func TestBuildPendingPoolStatusesSortOrder(t *testing.T) {
	classes := map[string]ClassSpec{
		"A": {Cluster: "default", Pool: "z"},
		"B": {Cluster: "default", Pool: "b"},
		"C": {Cluster: "default", Pool: "a"},
	}
	clusters := map[string]ClusterSpec{
		"us": {ID: "us"},
		"eu": {ID: "eu"},
	}
	got := buildPendingPoolStatuses(classes, clusters)
	pairs := make([]string, 0, len(got))
	for _, p := range got {
		pairs = append(pairs, p.status.Cluster+"/"+p.status.Name)
	}
	want := []string{
		"eu/a", "eu/b", "eu/z",
		"us/a", "us/b", "us/z",
	}
	if !sort.StringsAreSorted(pairs) || len(pairs) != len(want) {
		t.Fatalf("not sorted or wrong length: %v", pairs)
	}
	for i := range want {
		if pairs[i] != want[i] {
			t.Errorf("[%d] got %q want %q", i, pairs[i], want[i])
		}
	}
}
