package data

import (
	"context"
	"io"
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
