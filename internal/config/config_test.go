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
	t.Run("negative max retries", func(t *testing.T) {
		c := S3BackendConfig{Bucket: "b", Region: "r", MaxRetries: -1}
		if err := c.validate(); err == nil {
			t.Fatal("want error for negative max retries")
		}
	})
	t.Run("negative op timeout secs", func(t *testing.T) {
		c := S3BackendConfig{Bucket: "b", Region: "r", OpTimeoutSecs: -1}
		if err := c.validate(); err == nil {
			t.Fatal("want error for negative op timeout secs")
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

// TestLoadAcceptsS3RetryAndTimeoutEnvs pins US-006 env-var wiring:
// STRATA_S3_BACKEND_MAX_RETRIES + STRATA_S3_BACKEND_OP_TIMEOUT_SECS
// flow into S3BackendConfig.MaxRetries / OpTimeoutSecs and survive
// validate(). Zero/unset still loads cleanly (defaults applied at
// s3.Open).
func TestLoadAcceptsS3RetryAndTimeoutEnvs(t *testing.T) {
	t.Setenv("STRATA_DATA_BACKEND", "s3")
	t.Setenv("STRATA_S3_BACKEND_BUCKET", "strata-backend")
	t.Setenv("STRATA_S3_BACKEND_REGION", "us-east-1")
	t.Setenv("STRATA_S3_BACKEND_MAX_RETRIES", "9")
	t.Setenv("STRATA_S3_BACKEND_OP_TIMEOUT_SECS", "45")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.S3Backend.MaxRetries != 9 {
		t.Fatalf("max_retries: want 9, got %d", cfg.S3Backend.MaxRetries)
	}
	if cfg.S3Backend.OpTimeoutSecs != 45 {
		t.Fatalf("op_timeout_secs: want 45, got %d", cfg.S3Backend.OpTimeoutSecs)
	}
}

// TestS3BackendValidateSSEMode pins US-013 fail-fast on the SSE mode
// whitelist: empty (default) + the three documented values pass; anything
// else fails Load() with a clear error.
func TestS3BackendValidateSSEMode(t *testing.T) {
	cases := []struct {
		mode    string
		wantErr bool
	}{
		{"", false},
		{"passthrough", false},
		{"strata", false},
		{"both", false},
		{"AES256", true},
		{"on", true},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			c := S3BackendConfig{Bucket: "b", Region: "r", SSEMode: tc.mode}
			err := c.validate()
			if tc.wantErr && err == nil {
				t.Fatalf("mode %q: want error, got nil", tc.mode)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("mode %q: want nil error, got %v", tc.mode, err)
			}
		})
	}
}

// TestLoadAcceptsS3SSEEnvs pins US-013 env-var wiring:
// STRATA_S3_BACKEND_SSE_MODE + STRATA_S3_BACKEND_SSE_KMS_KEY_ID flow
// through into S3BackendConfig.
func TestLoadAcceptsS3SSEEnvs(t *testing.T) {
	t.Setenv("STRATA_DATA_BACKEND", "s3")
	t.Setenv("STRATA_S3_BACKEND_BUCKET", "strata-backend")
	t.Setenv("STRATA_S3_BACKEND_REGION", "us-east-1")
	t.Setenv("STRATA_S3_BACKEND_SSE_MODE", "both")
	t.Setenv("STRATA_S3_BACKEND_SSE_KMS_KEY_ID", "arn:aws:kms:us-east-1:111122223333:key/abc")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.S3Backend.SSEMode != "both" {
		t.Fatalf("sse_mode: want both, got %q", cfg.S3Backend.SSEMode)
	}
	if cfg.S3Backend.SSEKMSKeyID != "arn:aws:kms:us-east-1:111122223333:key/abc" {
		t.Fatalf("sse_kms_key_id: want arn..., got %q", cfg.S3Backend.SSEKMSKeyID)
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
