package s3

import (
	"context"
	"testing"
)

// TestDataHealthPropagatesClusterPerClass exercises a two-class mapping
// where each class lives on a distinct cluster id and asserts the
// PoolStatus.Cluster field propagates onto the wire report. Guards
// US-001 (placement-ui) — the Storage page's per-cluster split breaks
// when the field drops to "".
func TestDataHealthPropagatesClusterPerClass(t *testing.T) {
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
	if len(report.Pools) != 2 {
		t.Fatalf("want 2 pool rows, got %d (%+v)", len(report.Pools), report.Pools)
	}
	want := map[string]string{
		"HOT":  "eu",
		"COLD": "us",
	}
	for _, p := range report.Pools {
		wantCluster, ok := want[p.Class]
		if !ok {
			t.Errorf("unexpected class on row: %+v", p)
			continue
		}
		if p.Cluster != wantCluster {
			t.Errorf("Class=%s Cluster=%q want %q", p.Class, p.Cluster, wantCluster)
		}
	}
}
