package config

import (
	"strings"
	"testing"
)

// TestS3BackendValidateRequiresBucketAndRegion pins the US-005 fail-fast
// contract: when DataBackend=s3, missing bucket or region must error at
// Load() (validate path), never silently at first request.
func TestS3BackendValidateRequiresBucketAndRegion(t *testing.T) {
	t.Run("missing bucket", func(t *testing.T) {
		c := &Config{
			DataBackend:         "s3",
			MetaBackend:         "memory",
			DefaultBucketShards: 1,
			S3Backend: S3BackendConfig{
				Region: "us-east-1",
			},
		}
		err := c.validate()
		if err == nil || !strings.Contains(err.Error(), "STRATA_S3_BACKEND_BUCKET") {
			t.Fatalf("missing bucket: want error mentioning STRATA_S3_BACKEND_BUCKET, got %v", err)
		}
	})

	t.Run("missing region", func(t *testing.T) {
		c := &Config{
			DataBackend:         "s3",
			MetaBackend:         "memory",
			DefaultBucketShards: 1,
			S3Backend: S3BackendConfig{
				Bucket: "strata-backend",
			},
		}
		err := c.validate()
		if err == nil || !strings.Contains(err.Error(), "STRATA_S3_BACKEND_REGION") {
			t.Fatalf("missing region: want error mentioning STRATA_S3_BACKEND_REGION, got %v", err)
		}
	})
}

// TestS3BackendValidateCredsBothOrNeither pins the asymmetric-creds
// fail-fast: half-set static creds is misconfig (would fall back to SDK
// chain silently at runtime — operator footgun).
func TestS3BackendValidateCredsBothOrNeither(t *testing.T) {
	cases := []struct {
		name string
		cfg  S3BackendConfig
		want bool
	}{
		{"only access key", S3BackendConfig{Bucket: "b", Region: "r", AccessKey: "ak"}, true},
		{"only secret key", S3BackendConfig{Bucket: "b", Region: "r", SecretKey: "sk"}, true},
		{"both empty (SDK chain)", S3BackendConfig{Bucket: "b", Region: "r"}, false},
		{"both set", S3BackendConfig{Bucket: "b", Region: "r", AccessKey: "ak", SecretKey: "sk"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.validate()
			if tc.want && err == nil {
				t.Fatalf("want error, got nil")
			}
			if !tc.want && err != nil {
				t.Fatalf("want no error, got %v", err)
			}
		})
	}
}

// TestS3BackendValidateNonNegativeNumeric pins the numeric fail-fast:
// PartSize/UploadConcurrency must not be negative (zero is fine — Open
// substitutes the defaults).
func TestS3BackendValidateNonNegativeNumeric(t *testing.T) {
	t.Run("negative part size", func(t *testing.T) {
		c := S3BackendConfig{Bucket: "b", Region: "r", PartSize: -1}
		if err := c.validate(); err == nil {
			t.Fatal("want error for negative part size")
		}
	})
	t.Run("negative upload concurrency", func(t *testing.T) {
		c := S3BackendConfig{Bucket: "b", Region: "r", UploadConcurrency: -1}
		if err := c.validate(); err == nil {
			t.Fatal("want error for negative upload concurrency")
		}
	})
	t.Run("zero numerics OK (defaults applied at Open)", func(t *testing.T) {
		c := S3BackendConfig{Bucket: "b", Region: "r"}
		if err := c.validate(); err != nil {
			t.Fatalf("want nil error, got %v", err)
		}
	})
}

// TestLoadAcceptsS3DataBackend pins env-var wiring: setting
// STRATA_DATA_BACKEND=s3 + the required S3 vars yields a Config that
// validates. Memory-backed test — no network.
func TestLoadAcceptsS3DataBackend(t *testing.T) {
	t.Setenv("STRATA_DATA_BACKEND", "s3")
	t.Setenv("STRATA_S3_BACKEND_BUCKET", "strata-backend")
	t.Setenv("STRATA_S3_BACKEND_REGION", "us-east-1")
	t.Setenv("STRATA_S3_BACKEND_ENDPOINT", "http://minio:9000")
	t.Setenv("STRATA_S3_BACKEND_FORCE_PATH_STYLE", "true")
	t.Setenv("STRATA_S3_BACKEND_ACCESS_KEY", "ak")
	t.Setenv("STRATA_S3_BACKEND_SECRET_KEY", "sk")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DataBackend != "s3" {
		t.Fatalf("data_backend: want s3, got %q", cfg.DataBackend)
	}
	if cfg.S3Backend.Bucket != "strata-backend" {
		t.Fatalf("bucket: want strata-backend, got %q", cfg.S3Backend.Bucket)
	}
	if cfg.S3Backend.Region != "us-east-1" {
		t.Fatalf("region: want us-east-1, got %q", cfg.S3Backend.Region)
	}
	if cfg.S3Backend.Endpoint != "http://minio:9000" {
		t.Fatalf("endpoint: want http://minio:9000, got %q", cfg.S3Backend.Endpoint)
	}
	if !cfg.S3Backend.ForcePathStyle {
		t.Fatalf("force_path_style: want true, got false")
	}
	if cfg.S3Backend.AccessKey != "ak" || cfg.S3Backend.SecretKey != "sk" {
		t.Fatalf("creds: want ak/sk, got %q/%q", cfg.S3Backend.AccessKey, cfg.S3Backend.SecretKey)
	}
}

// TestLoadRejectsS3WithoutBucket pins the boot-time fail-fast: setting
// STRATA_DATA_BACKEND=s3 without the required bucket var must fail
// Load() with a clear message — the operator finds out at startup, not
// at first request.
func TestLoadRejectsS3WithoutBucket(t *testing.T) {
	t.Setenv("STRATA_DATA_BACKEND", "s3")
	t.Setenv("STRATA_S3_BACKEND_REGION", "us-east-1")
	// bucket intentionally unset

	_, err := Load()
	if err == nil {
		t.Fatal("want error from Load with empty bucket, got nil")
	}
	if !strings.Contains(err.Error(), "STRATA_S3_BACKEND_BUCKET") {
		t.Fatalf("want error mentioning STRATA_S3_BACKEND_BUCKET, got %v", err)
	}
}
