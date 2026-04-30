package s3

import (
	"net/http"
	"time"
)

// Config carries the wiring needed to talk to a single S3-compatible
// backend bucket. US-005 will populate it from STRATA_S3_BACKEND_* env
// vars via koanf; until then callers (tests, US-002 streaming PUT) build
// it directly.
type Config struct {
	// Endpoint is the full backend URL (http://host:port). Empty falls back
	// to the SDK's region-based AWS endpoint resolution.
	Endpoint string
	// Region is required (SDK validation).
	Region string
	// Bucket is the single backend bucket every Strata object lands in.
	Bucket string
	// AccessKey/SecretKey set static creds. Both empty falls through to the
	// SDK default credential chain (env / ~/.aws / IRSA / IMDS).
	AccessKey string
	SecretKey string
	// ForcePathStyle = true is required for MinIO + Ceph RGW; false for AWS.
	ForcePathStyle bool

	// PartSize is the multipart-upload part size in bytes. Zero ⇒
	// DefaultPartSize. Minimum honoured by the SDK is 5 MiB.
	PartSize int64
	// UploadConcurrency is the number of parallel part uploads per Put.
	// Zero ⇒ DefaultUploadConcurrency.
	UploadConcurrency int

	// HTTPClient overrides the SDK's default HTTP client. Optional —
	// production callers leave this nil. Tests may inject a counting
	// transport to assert per-op request counts (US-004 batch-delete).
	HTTPClient *http.Client

	// SkipProbe disables the boot-time writability probe (PutObject +
	// DeleteObject on ProbeKey, US-005). Production callers leave it
	// false — the probe catches read-only mounts, missing IAM
	// permissions, expired creds, and bucket-existence regressions
	// before the first real request. Tests that don't want the probe's
	// network round-trip flip this to true.
	SkipProbe bool

	// MaxRetries caps the SDK's adaptive-retry attempts per request
	// (initial attempt + N-1 retries). Zero ⇒ DefaultMaxRetries (5).
	// US-006: bounds tail latency under transient backend slowdowns.
	MaxRetries int

	// OpTimeout is the per-op deadline for short ops (Get / GetRange /
	// DeleteObject / DeleteBatch / Probe). Zero ⇒ DefaultOpTimeout
	// (30 s). US-006: a stuck request never hangs forever.
	OpTimeout time.Duration

	// MultipartTimeout is the per-Put deadline. Covers the full
	// multipart lifecycle (Create + Part uploads + Complete). Zero ⇒
	// DefaultMultipartTimeout (10 min). US-006: 10 min is generous for
	// a single object since manager.Uploader already retries each
	// part on transient failures.
	MultipartTimeout time.Duration
}

// ProbeKey is the sentinel object used by the boot-time writability
// probe (US-005). Strata writes + deletes this key against the backend
// bucket once on startup; it never appears during steady-state traffic.
const ProbeKey = ".strata-readyz-canary"

const (
	// DefaultPartSize matches PRD US-002: 16 MiB part size keeps the memory
	// bound at PartSize * UploadConcurrency = 64 MiB peak with defaults.
	DefaultPartSize int64 = 16 * 1024 * 1024
	// DefaultUploadConcurrency is the per-Put parallelism for multipart
	// part uploads. PRD US-002 default = 4.
	DefaultUploadConcurrency = 4
	// DefaultMaxRetries matches PRD US-006: max attempts = 5.
	DefaultMaxRetries = 5
	// DefaultOpTimeout matches PRD US-006: 30 s for small ops.
	DefaultOpTimeout = 30 * time.Second
	// DefaultMultipartTimeout matches PRD US-006: 10 min for the whole
	// multipart Put lifecycle (init + parts + complete).
	DefaultMultipartTimeout = 10 * time.Minute
)
