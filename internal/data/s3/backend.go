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
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go/logging"
	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/data"
)

// BackendName is the canonical Manifest.BackendRef.Backend value emitted by
// the s3-over-s3 data backend. Stored on every manifest produced by
// PutChunks so future code paths can branch on backend identity (the
// rados/memory paths leave BackendRef nil; chunks-shape manifests carry
// no Backend label).
const BackendName = "s3"

// Backend is the S3-over-S3 data backend. A zero-value / New() Backend is
// stub-only (every method returns errors.ErrUnsupported); a Backend built
// via Open carries a live S3 client + multipart Uploader.
type Backend struct {
	bucket           string
	client           *awss3.Client
	uploader         *manager.Uploader
	opTimeout        time.Duration
	multipartTimeout time.Duration
	// sseMode is one of data.SSEMode{Passthrough,Strata,Both}; never the
	// empty string after Open (defaults to passthrough). Used by
	// Put/PutChunks/CreateBackendMultipart to decide whether to set the
	// backend ServerSideEncryption header and what to record on the
	// Manifest.SSE.
	sseMode string
	// sseKMSKeyID resolves aws:kms over AES256 in passthrough/both. Empty
	// (default) keeps AES256 / SSE-S3.
	sseKMSKeyID string
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

	// US-007: register metrics on the default Prometheus registry the
	// first time a live backend is constructed. Idempotent via sync.Once
	// so the rados-only path never pays for s3-specific collectors.
	RegisterMetrics()

	maxRetries := cfg.MaxRetries
	if maxRetries <= 0 {
		maxRetries = DefaultMaxRetries
	}
	opTimeout := cfg.OpTimeout
	if opTimeout <= 0 {
		opTimeout = DefaultOpTimeout
	}
	multipartTimeout := cfg.MultipartTimeout
	if multipartTimeout <= 0 {
		multipartTimeout = DefaultMultipartTimeout
	}
	sseMode := cfg.SSEMode
	if sseMode == "" {
		sseMode = data.SSEModePassthrough
	}
	switch sseMode {
	case data.SSEModePassthrough, data.SSEModeStrata, data.SSEModeBoth:
	default:
		return nil, fmt.Errorf("s3: STRATA_S3_BACKEND_SSE_MODE must be one of {passthrough, strata, both}, got %q", sseMode)
	}

	loadOpts := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(cfg.Region),
		// Adaptive retry mode rate-limits client-side under sustained
		// 503 SlowDown / 429 TooManyRequests pressure (US-006). The
		// SDK's standard retryable-status set covers 5xx + 429 + network
		// errors and excludes 4xx auth/not-found — matches AC.
		awsconfig.WithRetryMode(aws.RetryModeAdaptive),
		awsconfig.WithRetryMaxAttempts(maxRetries),
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
		// LogRetries makes the SDK emit a per-retry log line carrying
		// service + operation + attempt number (US-006). Wrap the
		// SDK's logger to promote those lines to slog Warn level so
		// they reach the operator's standard log pipeline rather than
		// hiding at SDK Debug.
		o.ClientLogMode |= aws.LogRetries
		o.Logger = retryWarnLogger{inner: o.Logger}
		// US-007: per-op latency + status + retry-pressure metrics via
		// a single Initialize-step observer that brackets the full op
		// lifecycle (serialize → retry → send → deserialize).
		o.APIOptions = append(o.APIOptions, instrumentStack)
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

	b := &Backend{
		bucket:           cfg.Bucket,
		client:           client,
		uploader:         uploader,
		opTimeout:        opTimeout,
		multipartTimeout: multipartTimeout,
		sseMode:          sseMode,
		sseKMSKeyID:      cfg.SSEKMSKeyID,
	}

	if !cfg.SkipProbe {
		if err := b.Probe(ctx); err != nil {
			return nil, err
		}
	}

	return b, nil
}

// Probe is the boot-time writability check (US-005). It does PutObject
// followed by DeleteObject on ProbeKey against the configured bucket —
// catches read-only mounts, missing IAM permissions, expired creds, and
// bucket-existence regressions before the gateway accepts traffic. The
// probe is invoked once during Open; tests that don't want network
// traffic set Config.SkipProbe = true.
//
// Failure-mode error messages name the failing op (probe-put / probe-
// delete) and include the bucket name + underlying SDK error so the
// operator gets a clear actionable signal at the gateway log.
func (b *Backend) Probe(ctx context.Context) error {
	if b.client == nil {
		return errors.ErrUnsupported
	}
	bucket := b.bucket
	key := ProbeKey
	putCtx, putCancel := b.opCtx(ctx)
	out, err := b.client.PutObject(putCtx, &awss3.PutObjectInput{
		Bucket: &bucket,
		Key:    &key,
		Body:   bytes.NewReader(nil),
	})
	putCancel()
	if err != nil {
		return fmt.Errorf("s3: probe-put bucket=%s key=%s: %w", bucket, key, err)
	}
	del := &awss3.DeleteObjectInput{Bucket: &bucket, Key: &key}
	// On versioning-enabled buckets, plain DeleteObject would leave a
	// delete-marker behind for the canary key. Pass the captured
	// VersionId so the probe leaves the bucket exactly as it found it.
	if out.VersionId != nil && *out.VersionId != "" {
		v := *out.VersionId
		del.VersionId = &v
	}
	delCtx, delCancel := b.opCtx(ctx)
	defer delCancel()
	if _, err := b.client.DeleteObject(delCtx, del); err != nil {
		return fmt.Errorf("s3: probe-delete bucket=%s key=%s: %w", bucket, key, err)
	}
	return nil
}

// opCtx returns ctx wrapped with the per-op short timeout (US-006). Used
// by Get / GetRange / DeleteObject / DeleteBatch / Probe sub-ops. When
// b.opTimeout is zero (only possible on a stub Backend that never went
// through Open) the parent context is returned unchanged.
func (b *Backend) opCtx(parent context.Context) (context.Context, context.CancelFunc) {
	if b.opTimeout <= 0 {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, b.opTimeout)
}

// uploadCtx returns ctx wrapped with the multipart-upload deadline
// (US-006). Used by Put which routes through manager.Uploader and may
// span init + parts + complete on large objects.
func (b *Backend) uploadCtx(parent context.Context) (context.Context, context.CancelFunc) {
	if b.multipartTimeout <= 0 {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, b.multipartTimeout)
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
//
// US-013: when the Backend's sseMode is passthrough or both, the SDK adds
// the configured ServerSideEncryption header (AES256 by default; aws:kms
// with sseKMSKeyID when set). In strata mode no SSE header is sent.
func (b *Backend) Put(ctx context.Context, oid string, r io.Reader, size int64) (*PutResult, error) {
	if b.uploader == nil {
		return nil, errors.ErrUnsupported
	}
	bucket := b.bucket
	key := oid
	in := &awss3.PutObjectInput{
		Bucket: &bucket,
		Key:    &key,
		Body:   r,
	}
	b.applyPutSSE(in)
	upCtx, cancel := b.uploadCtx(ctx)
	defer cancel()
	out, err := b.uploader.Upload(upCtx, in)
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

// applyPutSSE attaches the configured backend-SSE header fields onto a
// PutObjectInput when Backend.sseMode is passthrough or both. No-op in
// strata mode and when the mode resolves to empty (defensive — should not
// happen post-Open).
func (b *Backend) applyPutSSE(in *awss3.PutObjectInput) {
	if !b.backendSSEActive() {
		return
	}
	if b.sseKMSKeyID != "" {
		in.ServerSideEncryption = s3types.ServerSideEncryptionAwsKms
		k := b.sseKMSKeyID
		in.SSEKMSKeyId = &k
		return
	}
	in.ServerSideEncryption = s3types.ServerSideEncryptionAes256
}

// applyMultipartSSE is the CreateMultipartUploadInput sibling of
// applyPutSSE. UploadPart cannot carry SSE-S3/SSE-KMS headers — the
// backend inherits them from the multipart init, so the per-part path
// stays SSE-free.
func (b *Backend) applyMultipartSSE(in *awss3.CreateMultipartUploadInput) {
	if !b.backendSSEActive() {
		return
	}
	if b.sseKMSKeyID != "" {
		in.ServerSideEncryption = s3types.ServerSideEncryptionAwsKms
		k := b.sseKMSKeyID
		in.SSEKMSKeyId = &k
		return
	}
	in.ServerSideEncryption = s3types.ServerSideEncryptionAes256
}

// backendSSEActive returns true when the configured mode means the s3
// backend must send a ServerSideEncryption header to the backing bucket.
// Passthrough + both both send the header; strata does not.
func (b *Backend) backendSSEActive() bool {
	return b.sseMode == data.SSEModePassthrough || b.sseMode == data.SSEModeBoth
}

// manifestSSE builds the SSEInfo snapshot persisted on Manifest.SSE at
// write time (US-013). Mirrors the algorithm/key-id chosen by
// applyPutSSE so the GET path can branch per-object on the recorded mode
// rather than the live backend config (which may have been re-toggled).
//
// Returns nil when no SSE state is meaningful — currently only when the
// backend is unconfigured (zero-value, never reached after Open). All
// three modes record at least Mode so future migrations can read it back.
func (b *Backend) manifestSSE() *data.SSEInfo {
	if b.sseMode == "" {
		return nil
	}
	info := &data.SSEInfo{Mode: b.sseMode}
	if b.backendSSEActive() {
		if b.sseKMSKeyID != "" {
			info.Algorithm = data.SSEAlgorithmKMS
			info.KMSKeyID = b.sseKMSKeyID
		} else {
			info.Algorithm = data.SSEAlgorithmAES256
		}
	}
	return info
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
	opCtx, cancel := b.opCtx(ctx)
	out, err := b.client.GetObject(opCtx, &awss3.GetObjectInput{
		Bucket: &bucket,
		Key:    &key,
	})
	if err != nil {
		cancel()
		return nil, mapGetError(oid, err)
	}
	return &cancelOnCloseReader{ReadCloser: out.Body, cancel: cancel}, nil
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
	opCtx, cancel := b.opCtx(ctx)
	out, err := b.client.GetObject(opCtx, &awss3.GetObjectInput{
		Bucket: &bucket,
		Key:    &key,
		Range:  &rangeHeader,
	})
	if err != nil {
		cancel()
		return nil, mapGetError(oid, err)
	}
	return &cancelOnCloseReader{ReadCloser: out.Body, cancel: cancel}, nil
}

// retryWarnLogger wraps the SDK's aws.Logger so retry attempt lines
// (emitted by the SDK's retry middleware at Debug) surface in our
// slog pipeline at Warn — matching US-006 AC. Non-retry SDK log
// lines pass through unchanged. The retry message format is
// `retrying request <service>/<operation>, attempt N` per
// aws/retry/middleware.go; we filter on that prefix and re-emit.
type retryWarnLogger struct {
	inner logging.Logger
}

func (l retryWarnLogger) Logf(c logging.Classification, format string, args ...any) {
	if strings.HasPrefix(format, "retrying request") {
		slog.Warn("s3 backend retry: " + fmt.Sprintf(format, args...))
		return
	}
	if l.inner != nil {
		l.inner.Logf(c, format, args...)
	}
}

// cancelOnCloseReader pairs an SDK response Body with the per-op
// context.CancelFunc so the caller's Close() releases the context. The
// per-op timeout (US-006) is still ticking until the body is closed —
// production callers should size STRATA_S3_BACKEND_OP_TIMEOUT_SECS for
// the worst-case body-read deadline, not just the SDK round-trip.
type cancelOnCloseReader struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelOnCloseReader) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
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
	opCtx, cancel := b.opCtx(ctx)
	defer cancel()
	if _, err := b.client.DeleteObject(opCtx, in); err != nil {
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
		opCtx, cancel := b.opCtx(ctx)
		out, err := b.client.DeleteObjects(opCtx, &awss3.DeleteObjectsInput{
			Bucket: &bucket,
			Delete: &s3types.Delete{Objects: ids, Quiet: &quiet},
		})
		cancel()
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

// PutChunks streams r straight into the backend bucket as ONE object
// (US-009 native shape — Strata object = backend S3 object) and returns a
// Manifest with BackendRef populated and Chunks left empty per the
// 1:1 invariant documented on data.Manifest.
//
// Object key format: <bucket-uuid>/<object-uuid>. The bucket-uuid is read
// from data.WithBucketID(ctx, b.ID); when absent the prefix falls back to a
// random UUID — both shapes give random-prefix distribution that
// AWS S3's automatic prefix partitioning needs to avoid hot-prefix
// throttling. Per-bucket grouping (`aws s3 ls s3://<bb>/<bucket-uuid>/`)
// is forensic bonus when the bucket id is threaded through.
//
// Manifest.ETag carries the backend object's ETag verbatim (single-shot
// PutObject ETag = MD5; multipart ETag = composite hash-of-hashes-suffix —
// gateway clients understand both shapes). VersionID carries the SDK
// response VersionId verbatim with the three-state semantics from
// data.BackendRef ("" / "null" / <uuid>).
func (b *Backend) PutChunks(ctx context.Context, r io.Reader, class string) (*data.Manifest, error) {
	if b.uploader == nil {
		return nil, errors.ErrUnsupported
	}
	if class == "" {
		class = "STANDARD"
	}
	key := b.objectKey(ctx)

	// manager.UploadOutput does not surface the streamed byte count, so
	// wrap the reader to count. Used as the manifest Size — reading the
	// SDK response's Content-Length header is not enough because the
	// uploader can split the request into multiple part uploads on large
	// objects.
	cr := &countingReader{r: r}
	res, err := b.Put(ctx, key, cr, 0)
	if err != nil {
		return nil, err
	}

	m := &data.Manifest{
		Class:     class,
		Size:      cr.n,
		ChunkSize: data.DefaultChunkSize,
		ETag:      res.ETag,
		BackendRef: &data.BackendRef{
			Backend:   BackendName,
			Key:       key,
			ETag:      res.ETag,
			Size:      cr.n,
			VersionID: res.VersionID,
		},
		SSE: b.manifestSSE(),
	}
	return m, nil
}

// GetChunks streams [offset, offset+length) of the manifest's backend
// object back to the caller. The s3 backend serves only BackendRef-shape
// manifests — feeding it a chunks-shape manifest (rados-shape) returns
// errors.ErrUnsupported, which surfaces as a 500 at the gateway.
func (b *Backend) GetChunks(ctx context.Context, m *data.Manifest, offset, length int64) (io.ReadCloser, error) {
	if b.client == nil {
		return nil, errors.ErrUnsupported
	}
	if m == nil || m.BackendRef == nil {
		return nil, errors.ErrUnsupported
	}
	if offset < 0 || offset > m.Size {
		return nil, fmt.Errorf("s3: offset %d out of range (size %d)", offset, m.Size)
	}
	if length <= 0 || offset+length > m.Size {
		length = m.Size - offset
	}
	if length == 0 {
		return io.NopCloser(strings.NewReader("")), nil
	}
	return b.GetRange(ctx, m.BackendRef.Key, offset, length)
}

// Delete removes the manifest's backend object via DeleteObject. When
// BackendRef.VersionID is set, the SDK issues a versioned delete so
// versioning-enabled backends do not accumulate delete-markers (US-008
// defensive design). Idempotent — NoSuchKey is success.
//
// Manifests without BackendRef (legacy/rados-shape) are no-ops here:
// chunks-shape manifests are deleted via the rados backend's Delete on
// its own client. The s3 backend never sees rados chunks, but defensive
// nil-check keeps the contract clean.
func (b *Backend) Delete(ctx context.Context, m *data.Manifest) error {
	if b.client == nil {
		return errors.ErrUnsupported
	}
	if m == nil || m.BackendRef == nil {
		return nil
	}
	return b.DeleteObject(ctx, m.BackendRef.Key, m.BackendRef.VersionID)
}

func (b *Backend) Close() error { return nil }

// objectKey builds the backend object key. Format <bucket-uuid>/<object-
// uuid> per US-009 — UUID-shaped prefix gives random distribution for
// AWS-side automatic prefix partitioning; per-bucket grouping is
// forensic bonus when callers thread bucket id via data.WithBucketID.
func (b *Backend) objectKey(ctx context.Context) string {
	objectID := uuid.NewString()
	if bucketID, ok := data.BucketIDFromContext(ctx); ok {
		return bucketID.String() + "/" + objectID
	}
	return uuid.NewString() + "/" + objectID
}

// countingReader wraps io.Reader to tally bytes seen by PutChunks. The
// manager.Uploader streams the bytes through this wrapper on every Read,
// so n reflects the full uploaded length when Put returns.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
