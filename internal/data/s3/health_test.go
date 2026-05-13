package s3

import (
	"context"
	"sort"
	"testing"
)

// TestDataHealthCrossProductBuckets exercises a two-cluster × two-bucket
// matrix and asserts DataHealth emits one row per (cluster, bucket)
// cell — 4 rows total. Guards US-001 of drain-lifecycle: the Pools
// table reflects actual per-cluster distribution instead of the class
// env routing config.
func TestDataHealthCrossProductBuckets(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "ak")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "sk")

	eu := newCapturingS3Server(t)
	us := newCapturingS3Server(t)
	t.Cleanup(eu.Close)
	t.Cleanup(us.Close)

	cfg := Config{
		Clusters: map[string]S3ClusterSpec{
			"eu": {Endpoint: eu.URL(), Region: "eu-west-1", ForcePathStyle: true, Credentials: CredentialsRef{Type: CredentialsChain}},
			"us": {Endpoint: us.URL(), Region: "us-east-1", ForcePathStyle: true, Credentials: CredentialsRef{Type: CredentialsChain}},
		},
		Classes: map[string]ClassSpec{
			"HOT":  {Cluster: "eu", Bucket: "hot-eu"},
			"COLD": {Cluster: "us", Bucket: "cold-us"},
		},
		SkipCredsCheck: true,
	}
	b, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	report, err := b.DataHealth(context.Background())
	if err != nil {
		t.Fatalf("DataHealth: %v", err)
	}
	if report == nil {
		t.Fatal("DataHealth: nil report")
	}
	if len(report.Pools) != 4 {
		t.Fatalf("want 4 pool rows (2 clusters × 2 buckets), got %d (%+v)", len(report.Pools), report.Pools)
	}
	byCluster := map[string]int{}
	pairs := make([]string, 0, len(report.Pools))
	for _, p := range report.Pools {
		byCluster[p.Cluster]++
		pairs = append(pairs, p.Cluster+"/"+p.Name)
	}
	if byCluster["eu"] != 2 || byCluster["us"] != 2 {
		t.Errorf("per-cluster count mismatch: %v want eu=2 us=2", byCluster)
	}
	if !sort.StringsAreSorted(pairs) {
		t.Errorf("pools not sorted ascending by (Cluster, Name): %v", pairs)
	}
}

// TestDataHealthGroupsClassesPerBucket asserts that two classes mapped
// to the same bucket collapse the Class field on every (cluster,
// bucket) row into the sorted-comma-joined list of classes.
func TestDataHealthGroupsClassesPerBucket(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "ak")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "sk")

	srv := newCapturingS3Server(t)
	t.Cleanup(srv.Close)

	cfg := Config{
		Clusters: map[string]S3ClusterSpec{
			"default": {Endpoint: srv.URL(), Region: "us-east-1", ForcePathStyle: true, Credentials: CredentialsRef{Type: CredentialsChain}},
		},
		Classes: map[string]ClassSpec{
			"STANDARD":    {Cluster: "default", Bucket: "shared"},
			"STANDARD_IA": {Cluster: "default", Bucket: "shared"},
		},
		SkipCredsCheck: true,
	}
	b, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	report, err := b.DataHealth(context.Background())
	if err != nil {
		t.Fatalf("DataHealth: %v", err)
	}
	// 1 cluster × 1 distinct bucket = 1 row labelled with the joined
	// class list.
	if len(report.Pools) != 1 {
		t.Fatalf("want 1 pool row, got %d (%+v)", len(report.Pools), report.Pools)
	}
	if report.Pools[0].Class != "STANDARD,STANDARD_IA" {
		t.Errorf("Class=%q want STANDARD,STANDARD_IA", report.Pools[0].Class)
	}
}
