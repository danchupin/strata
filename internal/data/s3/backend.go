// Package s3 implements an S3-compatible data backend for Strata.
//
// US-001 laid down the package skeleton. US-002 adds the streaming Put
// path: a fully constructed Backend (built via Open) talks to any
// S3-compatible endpoint via aws-sdk-go-v2 and uploads bytes through
// feature/s3/manager.NewUploader — single-shot or multipart transparently.
//
// The data.Backend interface methods (PutChunks / GetChunks / Delete) stay
// stubs until US-009 wires the gateway dispatch and the manifest schema
// gains BackendRef (US-008).
package s3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/danchupin/strata/internal/data"
)

// Backend is the S3-over-S3 data backend. A zero-value / New() Backend is
// stub-only (every method returns errors.ErrUnsupported); a Backend built
// via Open carries a live S3 client + multipart Uploader.
type Backend struct {
	bucket   string
	client   *awss3.Client
	uploader *manager.Uploader
}

// New constructs a stub Backend with no live S3 client. Every method
// returns errors.ErrUnsupported. Kept for the US-001 contract; US-002+
// callers should use Open.
func New() *Backend {
	return &Backend{}
}

// Open builds a live Backend wired to the supplied S3 endpoint. Validates
// required config (Bucket, Region) and resolves credentials via the SDK
// default chain when AccessKey/SecretKey are empty.
func Open(ctx context.Context, cfg Config) (*Backend, error) {
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("s3: bucket required")
	}
	if cfg.Region == "" {
		return nil, fmt.Errorf("s3: region required")
	}

	loadOpts := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(cfg.Region),
	}
	if cfg.AccessKey != "" && cfg.SecretKey != "" {
		loadOpts = append(loadOpts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
		))
	}
	if cfg.HTTPClient != nil {
		loadOpts = append(loadOpts, awsconfig.WithHTTPClient(cfg.HTTPClient))
	}

	awscfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("s3: load aws config: %w", err)
	}

	client := awss3.NewFromConfig(awscfg, func(o *awss3.Options) {
		if cfg.Endpoint != "" {
			endpoint := cfg.Endpoint
			o.BaseEndpoint = &endpoint
		}
		o.UsePathStyle = cfg.ForcePathStyle
	})

	partSize := cfg.PartSize
	if partSize <= 0 {
		partSize = DefaultPartSize
	}
	concurrency := cfg.UploadConcurrency
	if concurrency <= 0 {
		concurrency = DefaultUploadConcurrency
	}

	uploader := manager.NewUploader(client, func(u *manager.Uploader) {
		u.PartSize = partSize
		u.Concurrency = concurrency
		// LeavePartsOnError defaults to false — manager calls
		// AbortMultipartUpload on context cancel / error so no orphan
		// multipart sessions leak in the backend bucket.
	})

	return &Backend{
		bucket:   cfg.Bucket,
		client:   client,
		uploader: uploader,
	}, nil
}

// Compile-time assertion that *Backend satisfies data.Backend.
var _ data.Backend = (*Backend)(nil)

// PutResult is returned by Backend.Put. ETag is the backend object ETag
// with surrounding quotes stripped. VersionID carries the SDK response
// VersionId verbatim — three-state semantics per PRD US-002:
//
//	""           backend has no versioning OR versioning off
//	"null"       versioning Suspended
//	<other>      UUID-shaped version-id from versioning-enabled bucket
type PutResult struct {
	ETag      string
	VersionID string
	Size      int64
}

// Put streams r into the backend bucket under key oid via the manager
// Uploader — single-shot PutObject for small objects, multipart for large
// ones (transparently). size is informational; the upload is bounded by
// the reader's EOF, not the size hint.
//
// Memory bound: PartSize * UploadConcurrency (default 64 MiB peak). On
// context cancel, manager.Uploader aborts the multipart so no orphan
// sessions are left in the backend bucket.
func (b *Backend) Put(ctx context.Context, oid string, r io.Reader, size int64) (*PutResult, error) {
	if b.uploader == nil {
		return nil, errors.ErrUnsupported
	}
	bucket := b.bucket
	key := oid
	out, err := b.uploader.Upload(ctx, &awss3.PutObjectInput{
		Bucket: &bucket,
		Key:    &key,
		Body:   r,
	})
	if err != nil {
		return nil, fmt.Errorf("s3: upload %s: %w", oid, err)
	}
	res := &PutResult{Size: size}
	if out.ETag != nil {
		res.ETag = strings.Trim(*out.ETag, `"`)
	}
	if out.VersionID != nil {
		res.VersionID = *out.VersionID
	}
	return res, nil
}

// Get streams the full backend object body for oid back to the caller.
// Returned ReadCloser wraps the SDK's HTTP response body — caller MUST
// Close. Backend NoSuchKey is mapped to data.ErrNotFound so the gateway
// surfaces a 404 NoSuchKey instead of a 500.
func (b *Backend) Get(ctx context.Context, oid string) (io.ReadCloser, error) {
	if b.client == nil {
		return nil, errors.ErrUnsupported
	}
	bucket := b.bucket
	key := oid
	out, err := b.client.GetObject(ctx, &awss3.GetObjectInput{
		Bucket: &bucket,
		Key:    &key,
	})
	if err != nil {
		return nil, mapGetError(oid, err)
	}
	return out.Body, nil
}

// GetRange streams [off, off+length) of the backend object body for oid.
// Issues GetObject with Range: bytes=<off>-<off+length-1>. Returned
// ReadCloser wraps the SDK's HTTP response body — caller MUST Close.
// Backend NoSuchKey is mapped to data.ErrNotFound.
func (b *Backend) GetRange(ctx context.Context, oid string, off, length int64) (io.ReadCloser, error) {
	if b.client == nil {
		return nil, errors.ErrUnsupported
	}
	if length <= 0 {
		return nil, fmt.Errorf("s3: range length must be positive, got %d", length)
	}
	if off < 0 {
		return nil, fmt.Errorf("s3: range offset must be non-negative, got %d", off)
	}
	bucket := b.bucket
	key := oid
	rangeHeader := fmt.Sprintf("bytes=%d-%d", off, off+length-1)
	out, err := b.client.GetObject(ctx, &awss3.GetObjectInput{
		Bucket: &bucket,
		Key:    &key,
		Range:  &rangeHeader,
	})
	if err != nil {
		return nil, mapGetError(oid, err)
	}
	return out.Body, nil
}

// mapGetError translates SDK errors that callers want to branch on into
// the data package's sentinels. Today only NoSuchKey is mapped; other
// errors are wrapped verbatim.
func mapGetError(oid string, err error) error {
	var noSuchKey *s3types.NoSuchKey
	if errors.As(err, &noSuchKey) {
		return fmt.Errorf("s3: get %s: %w", oid, data.ErrNotFound)
	}
	return fmt.Errorf("s3: get %s: %w", oid, err)
}

// ObjectRef identifies a single backend object for DeleteBatch. VersionID
// carries the same three-state semantics as PutResult.VersionID
// (US-002/US-008): "" = backend without versioning OR versioning off (plain
// delete); "null" = versioning Suspended; <uuid> = versioning enabled.
type ObjectRef struct {
	Key       string
	VersionID string
}

// DeleteFailure records a per-ref failure inside a DeleteBatch response.
// The transport-level error from DeleteBatch is the second return value;
// per-ref soft failures are returned in this slice (empty on full success).
type DeleteFailure struct {
	Ref ObjectRef
	Err error
}

// DeleteBatchLimit is the S3 protocol cap on objects per DeleteObjects
// request. DeleteBatch chunks the input slice at this boundary.
const DeleteBatchLimit = 1000

// DeleteObject removes a single backend object. When versionID == "" the
// SDK issues a plain DeleteObject (frees bytes immediately on
// non-versioned and suspended buckets, creates a delete-marker on
// versioning-enabled buckets — see US-008 defensive design notes). When
// versionID != "" the SDK issues a versioned DeleteObject (deletes the
// specific version, skips delete-marker creation on versioning-enabled
// backends; "null" cleans the suspended-bucket version slot).
//
// Idempotent: NoSuchKey from the backend is treated as success.
func (b *Backend) DeleteObject(ctx context.Context, oid, versionID string) error {
	if b.client == nil {
		return errors.ErrUnsupported
	}
	bucket := b.bucket
	key := oid
	in := &awss3.DeleteObjectInput{
		Bucket: &bucket,
		Key:    &key,
	}
	if versionID != "" {
		v := versionID
		in.VersionId = &v
	}
	if _, err := b.client.DeleteObject(ctx, in); err != nil {
		var noSuchKey *s3types.NoSuchKey
		if errors.As(err, &noSuchKey) {
			return nil
		}
		return fmt.Errorf("s3: delete %s: %w", oid, err)
	}
	return nil
}

// DeleteBatch removes up to len(refs) backend objects via the s3
// DeleteObjects API. Refs are chunked into DeleteBatchLimit-sized slices
// (S3 caps a single request at 1000 entries); each chunk is one HTTP
// request.
//
// Per-ref soft failures (e.g. AccessDenied on one key) come back in the
// failures slice without aborting subsequent batches. A transport-level
// error (network failure, signature mismatch, 5xx after retries) returns
// the failures collected so far + the error.
//
// Empty refs is a no-op (nil, nil).
func (b *Backend) DeleteBatch(ctx context.Context, refs []ObjectRef) ([]DeleteFailure, error) {
	if b.client == nil {
		return nil, errors.ErrUnsupported
	}
	if len(refs) == 0 {
		return nil, nil
	}
	bucket := b.bucket
	quiet := true
	var failures []DeleteFailure
	for start := 0; start < len(refs); start += DeleteBatchLimit {
		end := min(start+DeleteBatchLimit, len(refs))
		batch := refs[start:end]
		ids := make([]s3types.ObjectIdentifier, len(batch))
		for i, ref := range batch {
			key := ref.Key
			ids[i] = s3types.ObjectIdentifier{Key: &key}
			if ref.VersionID != "" {
				v := ref.VersionID
				ids[i].VersionId = &v
			}
		}
		out, err := b.client.DeleteObjects(ctx, &awss3.DeleteObjectsInput{
			Bucket: &bucket,
			Delete: &s3types.Delete{Objects: ids, Quiet: &quiet},
		})
		if err != nil {
			return failures, fmt.Errorf("s3: delete batch [%d:%d]: %w", start, end, err)
		}
		for _, e := range out.Errors {
			ref := ObjectRef{}
			if e.Key != nil {
				ref.Key = *e.Key
			}
			if e.VersionId != nil {
				ref.VersionID = *e.VersionId
			}
			code := ""
			msg := ""
			if e.Code != nil {
				code = *e.Code
			}
			if e.Message != nil {
				msg = *e.Message
			}
			// NoSuchKey on a per-ref entry is idempotent success — drop it.
			if code == "NoSuchKey" {
				continue
			}
			failures = append(failures, DeleteFailure{
				Ref: ref,
				Err: fmt.Errorf("s3: delete %s (version %q): %s: %s", ref.Key, ref.VersionID, code, msg),
			})
		}
	}
	return failures, nil
}

func (b *Backend) PutChunks(ctx context.Context, r io.Reader, class string) (*data.Manifest, error) {
	return nil, errors.ErrUnsupported
}

func (b *Backend) GetChunks(ctx context.Context, m *data.Manifest, offset, length int64) (io.ReadCloser, error) {
	return nil, errors.ErrUnsupported
}

func (b *Backend) Delete(ctx context.Context, m *data.Manifest) error {
	return errors.ErrUnsupported
}

func (b *Backend) Close() error { return nil }
