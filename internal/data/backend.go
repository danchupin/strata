package data

import (
	"context"
	"io"
	"time"
)

type Backend interface {
	PutChunks(ctx context.Context, r io.Reader, class string) (*Manifest, error)
	GetChunks(ctx context.Context, m *Manifest, offset, length int64) (io.ReadCloser, error)
	Delete(ctx context.Context, m *Manifest) error
	Close() error
}

// MultipartBackend is the optional capability surface for data backends that
// can map a Strata multipart upload 1:1 onto their own multipart protocol
// (US-010 S3-over-S3). Today only the s3 backend implements it; the gateway
// type-asserts at the multipart entry-points and falls through to the
// chunk-based path when the backend is not multipart-aware.
//
// CreateBackendMultipart returns an opaque handle that the gateway persists
// in meta.MultipartUpload.BackendUploadID. The backend encodes whatever it
// needs in that string (target object key, SDK upload-id) — the gateway
// treats it as an opaque token and replays it on UploadBackendPart,
// CompleteBackendMultipart, and AbortBackendMultipart.
type MultipartBackend interface {
	CreateBackendMultipart(ctx context.Context, class string) (handle string, err error)
	UploadBackendPart(ctx context.Context, handle string, partNumber int32, r io.Reader, size int64) (etag string, err error)
	CompleteBackendMultipart(ctx context.Context, handle string, parts []BackendCompletedPart, class string) (*Manifest, error)
	AbortBackendMultipart(ctx context.Context, handle string) error
}

// BackendCompletedPart is the per-part input to CompleteBackendMultipart.
// PartNumber is the same Strata-facing part number (1..10000); ETag is the
// per-part backend ETag captured at UploadBackendPart time.
type BackendCompletedPart struct {
	PartNumber int32
	ETag       string
}

// LifecycleBackend is the optional capability surface for data backends that
// support a native bucket-lifecycle protocol (US-014 S3-over-S3). The gateway
// type-asserts at every PutBucketLifecycle / DeleteBucketLifecycle entry-point
// and skips translation when the assertion fails — rados/memory backends keep
// running Strata's own lifecycle worker for everything.
//
// Translation is best-effort: rules whose StorageClass is not natively
// understood by the backend are reported in skippedRuleIDs, and Strata's
// worker keeps owning those transitions. Expirations + AbortIncompleteUpload
// rules always translate so the backend can clean up orphan bytes
// independently of Strata's GC.
//
// bucketPrefix scopes every emitted backend rule to a single Strata bucket —
// the s3 backend stores Strata objects under <bucket-uuid>/<object-uuid>, so
// the per-bucket lifecycle filter must be prefixed with <bucket-uuid>/ to
// avoid affecting other Strata buckets sharing the same backend bucket.
type LifecycleBackend interface {
	PutBackendLifecycle(ctx context.Context, bucketPrefix string, rules []LifecycleRule) (skippedRuleIDs []string, err error)
	DeleteBackendLifecycle(ctx context.Context, bucketPrefix string) error
}

// LifecycleRule is the backend-translation input for one Strata lifecycle
// rule. Only the subset of fields the s3 backend can faithfully translate is
// carried; richer Strata-only fields (NoncurrentVersion*, Tag filters)
// stay with the worker.
//
// TransitionStorageClass is the empty string when the rule has no Transition
// action (in that case TransitionDays is also zero). ExpirationDays == 0 means
// the rule has no Expiration action. AbortIncompleteUploadDays == 0 means the
// rule has no AbortIncompleteMultipartUpload action.
//
// At least one of (TransitionDays, ExpirationDays, AbortIncompleteUploadDays)
// must be non-zero for the rule to be translatable — empty rules are dropped
// at the s3api layer before reaching the backend.
type LifecycleRule struct {
	ID                        string
	Prefix                    string
	TransitionDays            int
	TransitionStorageClass    string
	ExpirationDays            int
	AbortIncompleteUploadDays int
}

// CORSBackend is the optional capability surface for data backends that
// support a native bucket-CORS protocol (US-015 S3-over-S3). The gateway
// type-asserts at every PutBucketCORS / GetBucketCORS / DeleteBucketCORS
// entry-point and skips translation when the assertion fails — rados/memory
// backends keep CORS purely in the meta layer.
//
// Strata-stored CORS is the source of truth for the Strata wire response;
// the backend mirror exists so preflight OPTIONS requests against
// presigned-URL responses (US-016) hit the backend with the right rules.
// On GetBackendCORS conflicts (same rule ID), Strata's stored config wins
// at the gateway-merge layer.
type CORSBackend interface {
	PutBackendCORS(ctx context.Context, rules []CORSRule) error
	GetBackendCORS(ctx context.Context) ([]CORSRule, error)
	DeleteBackendCORS(ctx context.Context) error
}

// PresignBackend is the optional capability surface for data backends that
// can mint a presigned URL pointing directly at backend storage (US-016
// S3-over-S3). When the gateway sees a presigned GET on a bucket whose
// BackendPresign flag is enabled, it asks the backend for a backend-
// credentialled URL and 307-redirects the client there — Strata stays out
// of the data path beyond signature validation. rados/memory backends
// don't implement this surface, so the type-assertion fails and the gateway
// falls through to its in-process serve path.
//
// expires is forwarded verbatim from the original presigned request's
// X-Amz-Expires; the backend uses this as the lifetime of the minted URL.
type PresignBackend interface {
	PresignGetObject(ctx context.Context, m *Manifest, expires time.Duration) (string, error)
}

// DataHealthReport is the operator-facing snapshot returned by HealthProbe.
// One row per backing pool / bucket. Warnings carry cluster-wide anomalies
// that don't fit on a per-pool row (RADOS HEALTH_WARN/HEALTH_ERR check
// summaries, S3 reachability errors, etc.).
type DataHealthReport struct {
	Backend  string       `json:"backend"`
	Pools    []PoolStatus `json:"pools"`
	Warnings []string     `json:"warnings,omitempty"`
}

// PoolStatus is a single pool / backend-bucket row in DataHealthReport.
// Class is the storage-class label from the configured classes map; for
// backends that map every class to one bucket / pool (s3, memory) it is
// the comma-joined list of classes. Cluster is the cluster id the pool /
// bucket lives on; rendered empty for the memory backend (single virtual
// cluster).
type PoolStatus struct {
	Name        string `json:"name"`
	Class       string `json:"class"`
	Cluster     string `json:"cluster,omitempty"`
	BytesUsed   uint64 `json:"bytes_used"`
	ObjectCount uint64 `json:"object_count"`
	NumReplicas int    `json:"num_replicas"`
	State       string `json:"state"`
}

// HealthProbe is the optional capability surface that lets the storage page
// (US-002 of the web-ui-storage-status cycle) render data backend pool
// topology. RADOS, s3, and the in-memory backend implement it; the gateway
// adminapi type-asserts the live Backend and surfaces the report at
// GET /admin/v1/storage/data.
type HealthProbe interface {
	DataHealth(ctx context.Context) (*DataHealthReport, error)
}

// ClusterStatsProbe is the optional capability surface for data backends
// that can return per-cluster fill telemetry (US-006 placement-rebalance).
// RADOS implements it via `MonCommand({"prefix":"df","format":"json"})`;
// memory + s3 backends do not implement it (RADOS-only safety rail). The
// rebalance worker type-asserts the live Backend and skips the
// target-full check when the assertion fails — equivalent to receiving
// ErrClusterStatsNotSupported.
type ClusterStatsProbe interface {
	ClusterStats(ctx context.Context, clusterID string) (usedBytes, totalBytes int64, err error)
}

// CORSRule is the backend-translation input for one bucket CORS rule. The
// shape mirrors S3's CORSRule directly so translation is field-for-field;
// the empty string ID is allowed (S3 accepts unnamed rules).
//
// MaxAgeSeconds == 0 means the backend uses its default (browsers cache the
// preflight result for the request's lifetime).
type CORSRule struct {
	ID             string
	AllowedMethods []string
	AllowedOrigins []string
	AllowedHeaders []string
	ExposeHeaders  []string
	MaxAgeSeconds  int
}
