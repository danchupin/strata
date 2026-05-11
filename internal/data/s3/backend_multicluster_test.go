package s3

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

// TestNewValidatesClusterRefs pins US-002 AC5: every class's Cluster
// must reference a known cluster id. A typo or missing entry must fail
// at New time, not later.
func TestNewValidatesClusterRefs(t *testing.T) {
	cfg := Config{
		Clusters: map[string]S3ClusterSpec{
			"primary": {Endpoint: "https://s3.example.com", Region: "us-east-1", Credentials: CredentialsRef{Type: CredentialsChain}},
		},
		Classes: map[string]ClassSpec{
			"STANDARD": {Cluster: "no-such-cluster", Bucket: "x"},
		},
		SkipCredsCheck: true,
	}
	if _, err := New(cfg); err == nil {
		t.Fatal("New with unknown cluster ref: want error, got nil")
	}
}

// TestNewRequiresClustersAndClasses pins US-002 AC5: a Config with no
// clusters or no classes errors at New.
func TestNewRequiresClustersAndClasses(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("New with empty config: want error, got nil")
	}
	if _, err := New(Config{
		Clusters: map[string]S3ClusterSpec{"x": {Endpoint: "https://s3", Region: "r", Credentials: CredentialsRef{Type: CredentialsChain}}},
	}); err == nil {
		t.Fatal("New without classes: want error, got nil")
	}
	if _, err := New(Config{
		Classes: map[string]ClassSpec{"STANDARD": {Cluster: "x", Bucket: "b"}},
	}); err == nil {
		t.Fatal("New without clusters: want error, got nil")
	}
}

// TestNewRejectsMissingEnvCredentials pins US-002 AC6: credentials of
// type env: must have both vars set at boot. New fails fast otherwise.
func TestNewRejectsMissingEnvCredentials(t *testing.T) {
	t.Setenv("STRATA_TEST_AK", "")
	t.Setenv("STRATA_TEST_SK", "")
	cfg := Config{
		Clusters: map[string]S3ClusterSpec{
			"primary": {
				Endpoint:    "https://s3.example.com",
				Region:      "us-east-1",
				Credentials: CredentialsRef{Type: CredentialsEnv, Ref: "STRATA_TEST_AK:STRATA_TEST_SK"},
			},
		},
		Classes: map[string]ClassSpec{
			"STANDARD": {Cluster: "primary", Bucket: "x"},
		},
	}
	if _, err := New(cfg); err == nil {
		t.Fatal("New with unset env-var creds: want error, got nil")
	}
}

// TestNewRejectsMissingCredentialsFile pins US-002 AC6: credentials of
// type file: must resolve to an existing path at boot.
func TestNewRejectsMissingCredentialsFile(t *testing.T) {
	cfg := Config{
		Clusters: map[string]S3ClusterSpec{
			"primary": {
				Endpoint:    "https://s3.example.com",
				Region:      "us-east-1",
				Credentials: CredentialsRef{Type: CredentialsFile, Ref: "/nonexistent/path/to/credentials"},
			},
		},
		Classes: map[string]ClassSpec{
			"STANDARD": {Cluster: "primary", Bucket: "x"},
		},
	}
	_, err := New(cfg)
	if err == nil {
		t.Fatal("New with missing creds file: want error, got nil")
	}
	if !strings.Contains(err.Error(), "credentials file") {
		t.Fatalf("error must mention credentials file, got %v", err)
	}
}

// TestNewAcceptsEnvCredentials pins the happy path: env-var creds with
// both vars set passes the boot-time check.
func TestNewAcceptsEnvCredentials(t *testing.T) {
	t.Setenv("STRATA_TEST_AK", "ak")
	t.Setenv("STRATA_TEST_SK", "sk")
	cfg := Config{
		Clusters: map[string]S3ClusterSpec{
			"primary": {
				Endpoint:    "https://s3.example.com",
				Region:      "us-east-1",
				Credentials: CredentialsRef{Type: CredentialsEnv, Ref: "STRATA_TEST_AK:STRATA_TEST_SK"},
			},
		},
		Classes: map[string]ClassSpec{
			"STANDARD": {Cluster: "primary", Bucket: "hot-tier"},
		},
	}
	b, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if len(b.clusters) != 1 {
		t.Fatalf("clusters: want 1, got %d", len(b.clusters))
	}
	if b.clusters["primary"].client != nil {
		t.Fatal("client must be nil before first connFor — lazy build broken")
	}
}

// TestResolveClassRoutesToClusterAndBucket pins AC4 (resolveClass).
func TestResolveClassRoutesToClusterAndBucket(t *testing.T) {
	cfg := Config{
		Clusters: map[string]S3ClusterSpec{
			"eu": {Endpoint: "https://eu.s3", Region: "eu-west-1", Credentials: CredentialsRef{Type: CredentialsChain}},
			"us": {Endpoint: "https://us.s3", Region: "us-east-1", Credentials: CredentialsRef{Type: CredentialsChain}},
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
	cluster, bucket, err := b.resolveClass("HOT")
	if err != nil {
		t.Fatalf("resolveClass(HOT): %v", err)
	}
	if cluster != "eu" || bucket != "hot-eu" {
		t.Fatalf("resolveClass(HOT) = (%q,%q), want (eu, hot-eu)", cluster, bucket)
	}
	cluster, bucket, err = b.resolveClass("COLD")
	if err != nil {
		t.Fatalf("resolveClass(COLD): %v", err)
	}
	if cluster != "us" || bucket != "cold-us" {
		t.Fatalf("resolveClass(COLD) = (%q,%q), want (us, cold-us)", cluster, bucket)
	}
	if _, _, err := b.resolveClass("MISSING"); !errors.Is(err, ErrUnknownStorageClass) {
		t.Fatalf("resolveClass(MISSING): want ErrUnknownStorageClass, got %v", err)
	}
}

// TestConnForCachesPerCluster pins AC3: connFor builds the client + the
// uploader once and reuses on the second call. Mutex contention isn't
// exercised here — the race detector covers that.
func TestConnForCachesPerCluster(t *testing.T) {
	cfg := Config{
		Clusters: map[string]S3ClusterSpec{
			"primary": {Endpoint: "https://s3.example.com", Region: "us-east-1", Credentials: CredentialsRef{Type: CredentialsChain}},
		},
		Classes: map[string]ClassSpec{
			"STANDARD": {Cluster: "primary", Bucket: "x"},
		},
		SkipCredsCheck: true,
	}
	b, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	c1, err := b.connFor(ctx, "primary")
	if err != nil {
		t.Fatalf("connFor first: %v", err)
	}
	if c1.client == nil || c1.uploader == nil {
		t.Fatal("connFor first: client/uploader must be wired")
	}
	c2, err := b.connFor(ctx, "primary")
	if err != nil {
		t.Fatalf("connFor second: %v", err)
	}
	if c1 != c2 || c1.client != c2.client {
		t.Fatal("connFor must cache per-cluster — got distinct entries on second call")
	}
}

// TestNewChainCredsSkipBootCheck pins the design decision (validate
// credentials at New time vs. defer to first connect): chain-shape creds
// always defer (IMDS / IRSA calls are too costly at boot). Even with
// SkipCredsCheck=false, New must succeed for chain.
func TestNewChainCredsSkipBootCheck(t *testing.T) {
	cfg := Config{
		Clusters: map[string]S3ClusterSpec{
			"primary": {Endpoint: "https://s3.example.com", Region: "us-east-1", Credentials: CredentialsRef{Type: CredentialsChain}},
		},
		Classes: map[string]ClassSpec{
			"STANDARD": {Cluster: "primary", Bucket: "x"},
		},
	}
	if _, err := New(cfg); err != nil {
		t.Fatalf("New with chain creds: want nil (deferred check), got %v", err)
	}
}

// TestValidateClusterCredentialsFileExists pins AC6 file-creds path:
// when the file exists, validation passes; the profile suffix is
// ignored for the existence check.
func TestValidateClusterCredentialsFileExists(t *testing.T) {
	tmp, err := os.CreateTemp(t.TempDir(), "creds")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	tmp.Close()
	cfg := Config{
		Clusters: map[string]S3ClusterSpec{
			"primary": {
				Endpoint:    "https://s3.example.com",
				Region:      "us-east-1",
				Credentials: CredentialsRef{Type: CredentialsFile, Ref: tmp.Name() + ":custom"},
			},
		},
		Classes: map[string]ClassSpec{
			"STANDARD": {Cluster: "primary", Bucket: "x"},
		},
	}
	if _, err := New(cfg); err != nil {
		t.Fatalf("New with existing creds file: want nil, got %v", err)
	}
}
