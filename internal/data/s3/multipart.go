package s3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/danchupin/strata/internal/data"
)

// handleSeparator joins a backend object key with a backend SDK upload-id
// inside the opaque BackendUploadID handle the gateway persists in
// meta.MultipartUpload.BackendUploadID. The byte is invalid in both an S3
// object key and an SDK-generated upload-id, so split is unambiguous.
const handleSeparator = "\x00"

// encodeHandle packs the backend object key + SDK upload-id into one
// opaque token. The gateway never inspects it.
func encodeHandle(key, uploadID string) string {
	return key + handleSeparator + uploadID
}

// decodeHandle splits a previously encoded handle. Returns an error when
// the handle was not produced by encodeHandle (defensive — guards against
// hand-crafted values that bypass the gateway).
func decodeHandle(h string) (key, uploadID string, err error) {
	parts := strings.SplitN(h, handleSeparator, 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("s3: malformed multipart handle")
	}
	return parts[0], parts[1], nil
}

// CreateBackendMultipart initiates a backend multipart upload at a
// gateway-allocated key (<bucket-uuid>/<object-uuid>) and returns an opaque
// handle the gateway persists for replay on UploadBackendPart /
// CompleteBackendMultipart / AbortBackendMultipart.
func (b *Backend) CreateBackendMultipart(ctx context.Context, class string) (string, error) {
	if b.client == nil {
		return "", errors.ErrUnsupported
	}
	if class == "" {
		class = "STANDARD"
	}
	bucket := b.bucket
	key := b.objectKey(ctx)
	opCtx, cancel := b.opCtx(ctx)
	defer cancel()
	out, err := b.client.CreateMultipartUpload(opCtx, &awss3.CreateMultipartUploadInput{
		Bucket: &bucket,
		Key:    &key,
	})
	if err != nil {
		return "", fmt.Errorf("s3: create multipart %s: %w", key, err)
	}
	if out.UploadId == nil || *out.UploadId == "" {
		return "", fmt.Errorf("s3: create multipart %s: SDK returned empty UploadId", key)
	}
	return encodeHandle(key, *out.UploadId), nil
}

// UploadBackendPart streams a single part body to the backend's UploadPart
// API and returns the per-part ETag (quote-stripped) the gateway must
// forward to the backend's CompleteMultipartUpload at finalisation.
//
// size is informational; the upload is bounded by the reader's EOF, not
// the size hint. The SDK transmits one HTTP request per UploadPart — no
// hidden re-buffering or chunking.
func (b *Backend) UploadBackendPart(ctx context.Context, handle string, partNumber int32, r io.Reader, size int64) (string, error) {
	if b.client == nil {
		return "", errors.ErrUnsupported
	}
	key, uploadID, err := decodeHandle(handle)
	if err != nil {
		return "", err
	}
	if partNumber < 1 || partNumber > 10000 {
		return "", fmt.Errorf("s3: part number %d out of range [1,10000]", partNumber)
	}
	bucket := b.bucket
	// UploadPart can run as long as the worst-case multipart timeout —
	// individual parts may be PartSize (16 MiB default) on a slow link.
	upCtx, cancel := b.uploadCtx(ctx)
	defer cancel()
	in := &awss3.UploadPartInput{
		Bucket:     &bucket,
		Key:        &key,
		UploadId:   &uploadID,
		PartNumber: &partNumber,
		Body:       r,
	}
	if size > 0 {
		in.ContentLength = &size
	}
	out, err := b.client.UploadPart(upCtx, in)
	if err != nil {
		return "", fmt.Errorf("s3: upload part %d (%s): %w", partNumber, key, err)
	}
	etag := ""
	if out.ETag != nil {
		etag = strings.Trim(*out.ETag, `"`)
	}
	if etag == "" {
		return "", fmt.Errorf("s3: upload part %d (%s): SDK returned empty ETag", partNumber, key)
	}
	return etag, nil
}

// CompleteBackendMultipart finalises the backend multipart upload and
// returns a BackendRef-shape Manifest pointing at the resulting backend
// object. parts must be sorted by PartNumber ascending and contain every
// uploaded part the client wants in the final object (S3 protocol
// requirement).
//
// The Manifest mirrors PutChunks's shape: Class+Size+ETag at the top level,
// BackendRef.{Backend, Key, ETag, Size, VersionID} populated, Chunks empty
// (US-008 1:1 invariant). Size is best-effort: the SDK's Complete response
// does not surface total bytes, so the gateway-supplied total is recorded.
func (b *Backend) CompleteBackendMultipart(ctx context.Context, handle string, parts []data.BackendCompletedPart, class string) (*data.Manifest, error) {
	if b.client == nil {
		return nil, errors.ErrUnsupported
	}
	if len(parts) == 0 {
		return nil, fmt.Errorf("s3: complete multipart: no parts supplied")
	}
	key, uploadID, err := decodeHandle(handle)
	if err != nil {
		return nil, err
	}
	if class == "" {
		class = "STANDARD"
	}
	completed := make([]s3types.CompletedPart, len(parts))
	for i, p := range parts {
		num := p.PartNumber
		etag := `"` + p.ETag + `"`
		completed[i] = s3types.CompletedPart{
			PartNumber: &num,
			ETag:       &etag,
		}
	}
	bucket := b.bucket
	// CompleteMultipartUpload can take minutes for large objects (the
	// backend assembles the final object server-side) — use the
	// multipart timeout, not the per-op timeout.
	upCtx, cancel := b.uploadCtx(ctx)
	defer cancel()
	out, err := b.client.CompleteMultipartUpload(upCtx, &awss3.CompleteMultipartUploadInput{
		Bucket:          &bucket,
		Key:             &key,
		UploadId:        &uploadID,
		MultipartUpload: &s3types.CompletedMultipartUpload{Parts: completed},
	})
	if err != nil {
		return nil, fmt.Errorf("s3: complete multipart %s: %w", key, err)
	}
	etag := ""
	if out.ETag != nil {
		etag = strings.Trim(*out.ETag, `"`)
	}
	versionID := ""
	if out.VersionId != nil {
		versionID = *out.VersionId
	}
	m := &data.Manifest{
		Class:     class,
		ChunkSize: data.DefaultChunkSize,
		ETag:      etag,
		BackendRef: &data.BackendRef{
			Backend:   BackendName,
			Key:       key,
			ETag:      etag,
			VersionID: versionID,
		},
	}
	return m, nil
}

// AbortBackendMultipart cancels an in-progress backend multipart upload.
// Idempotent: NoSuchUpload from the backend is treated as success so the
// gateway can call Abort defensively when meta state is inconsistent.
func (b *Backend) AbortBackendMultipart(ctx context.Context, handle string) error {
	if b.client == nil {
		return errors.ErrUnsupported
	}
	key, uploadID, err := decodeHandle(handle)
	if err != nil {
		return err
	}
	bucket := b.bucket
	opCtx, cancel := b.opCtx(ctx)
	defer cancel()
	if _, err := b.client.AbortMultipartUpload(opCtx, &awss3.AbortMultipartUploadInput{
		Bucket:   &bucket,
		Key:      &key,
		UploadId: &uploadID,
	}); err != nil {
		var noSuchUpload *s3types.NoSuchUpload
		if errors.As(err, &noSuchUpload) {
			return nil
		}
		return fmt.Errorf("s3: abort multipart %s: %w", key, err)
	}
	return nil
}

// Compile-time assertion that *Backend satisfies data.MultipartBackend.
var _ data.MultipartBackend = (*Backend)(nil)
