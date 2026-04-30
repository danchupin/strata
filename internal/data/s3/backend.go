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
