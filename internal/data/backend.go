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
