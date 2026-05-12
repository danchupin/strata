package rados

import (
	"sort"
	"testing"
)

// TestBuildPendingPoolStatusesTwoClusters asserts that DataHealth's
// cluster-grouping helper emits at least two distinct Cluster values
// when the classes map spans two clusters. Guards US-001 (placement-ui)
// — the UI splits the Pools table per cluster, so a regression that
// drops the Cluster field at the per-row level would crash the new
// surface.
func TestBuildPendingPoolStatusesTwoClusters(t *testing.T) {
	classes := map[string]ClassSpec{
		"STANDARD": {Cluster: "eu", Pool: "hot.eu", Namespace: ""},
		"COLD":     {Cluster: "us", Pool: "cold.us", Namespace: ""},
	}
	got := buildPendingPoolStatuses(classes)
	if len(got) != 2 {
		t.Fatalf("want 2 pool rows, got %d (%+v)", len(got), got)
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
// substitution from "" to DefaultCluster — config that leaves Cluster
// blank must still produce a populated Cluster field so the UI never
// renders "no cluster" for a RADOS pool.
func TestBuildPendingPoolStatusesEmptyClusterFallsBackToDefault(t *testing.T) {
	classes := map[string]ClassSpec{
		"STANDARD": {Cluster: "", Pool: "hot.pool"},
	}
	got := buildPendingPoolStatuses(classes)
	if len(got) != 1 {
		t.Fatalf("want 1 row, got %d", len(got))
	}
	if got[0].status.Cluster != DefaultCluster {
		t.Errorf("Cluster=%q want %q", got[0].status.Cluster, DefaultCluster)
	}
}

// TestBuildPendingPoolStatusesGroupsByPool asserts that two classes
// pointing at the same (cluster, pool, ns) collapse into one row whose
// Class field is the sorted-comma-joined list of classes.
func TestBuildPendingPoolStatusesGroupsByPool(t *testing.T) {
	classes := map[string]ClassSpec{
		"STANDARD":    {Cluster: "default", Pool: "hot.pool"},
		"STANDARD_IA": {Cluster: "default", Pool: "hot.pool"},
	}
	got := buildPendingPoolStatuses(classes)
	if len(got) != 1 {
		t.Fatalf("want 1 row, got %d (%+v)", len(got), got)
	}
	classesStr := got[0].status.Class
	if classesStr != "STANDARD,STANDARD_IA" {
		t.Errorf("Class=%q want STANDARD,STANDARD_IA", classesStr)
	}
}

// TestBuildPendingPoolStatusesSortOrder pins (cluster, pool, ns)
// ascending sort so the wire output is deterministic across renders.
func TestBuildPendingPoolStatusesSortOrder(t *testing.T) {
	classes := map[string]ClassSpec{
		"A": {Cluster: "us", Pool: "z"},
		"B": {Cluster: "eu", Pool: "b"},
		"C": {Cluster: "eu", Pool: "a"},
	}
	got := buildPendingPoolStatuses(classes)
	pairs := make([]string, 0, len(got))
	for _, p := range got {
		pairs = append(pairs, p.status.Cluster+"/"+p.status.Name)
	}
	want := []string{"eu/a", "eu/b", "us/z"}
	if !sort.StringsAreSorted(pairs) || len(pairs) != len(want) {
		t.Fatalf("not sorted: %v", pairs)
	}
	for i := range want {
		if pairs[i] != want[i] {
			t.Errorf("[%d] got %q want %q", i, pairs[i], want[i])
		}
	}
}
