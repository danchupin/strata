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
// meta.MultipartUpload.BackendUploadID.
const handleSeparator = "\x00"

func encodeHandle(key, uploadID string) string {
	return key + handleSeparator + uploadID
}

func decodeHandle(h string) (key, uploadID string, err error) {
	parts := strings.SplitN(h, handleSeparator, 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("s3: malformed multipart handle")
	}
	return parts[0], parts[1], nil
}

// CreateBackendMultipart initiates a backend multipart upload at a
// gateway-allocated key (<bucket-uuid>/<object-uuid>) and returns an
// opaque handle.
func (b *Backend) CreateBackendMultipart(ctx context.Context, class string) (string, error) {
	if class == "" {
		class = "STANDARD"
	}
	c, bucket, err := b.singleCluster(ctx)
	if err != nil {
		return "", err
	}
	key := b.objectKey(ctx)
	createIn := &awss3.CreateMultipartUploadInput{
		Bucket: &bucket,
		Key:    &key,
	}
	c.applyMultipartSSE(createIn)
	opCtx, cancel := opCtxFor(ctx, c.opTimeout)
	defer cancel()
	out, err := c.client.CreateMultipartUpload(opCtx, createIn)
	if err != nil {
		return "", fmt.Errorf("s3: create multipart %s: %w", key, err)
	}
	if out.UploadId == nil || *out.UploadId == "" {
		return "", fmt.Errorf("s3: create multipart %s: SDK returned empty UploadId", key)
	}
	return encodeHandle(key, *out.UploadId), nil
}

// UploadBackendPart streams a single part body to the backend's
// UploadPart API and returns the per-part ETag.
func (b *Backend) UploadBackendPart(ctx context.Context, handle string, partNumber int32, r io.Reader, size int64) (string, error) {
	key, uploadID, err := decodeHandle(handle)
	if err != nil {
		return "", err
	}
	if partNumber < 1 || partNumber > 10000 {
		return "", fmt.Errorf("s3: part number %d out of range [1,10000]", partNumber)
	}
	c, bucket, err := b.singleCluster(ctx)
	if err != nil {
		return "", err
	}
	upCtx, cancel := uploadCtxFor(ctx, c.multipartTimeout)
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
	out, err := c.client.UploadPart(upCtx, in)
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
// object.
func (b *Backend) CompleteBackendMultipart(ctx context.Context, handle string, parts []data.BackendCompletedPart, class string) (*data.Manifest, error) {
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
	c, bucket, err := b.singleCluster(ctx)
	if err != nil {
		return nil, err
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
	upCtx, cancel := uploadCtxFor(ctx, c.multipartTimeout)
	defer cancel()
	out, err := c.client.CompleteMultipartUpload(upCtx, &awss3.CompleteMultipartUploadInput{
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
		SSE: c.manifestSSE(),
	}
	return m, nil
}

// AbortBackendMultipart cancels an in-progress backend multipart upload.
// Idempotent: NoSuchUpload is treated as success.
func (b *Backend) AbortBackendMultipart(ctx context.Context, handle string) error {
	key, uploadID, err := decodeHandle(handle)
	if err != nil {
		return err
	}
	c, bucket, err := b.singleCluster(ctx)
	if err != nil {
		return err
	}
	opCtx, cancel := opCtxFor(ctx, c.opTimeout)
	defer cancel()
	if _, err := c.client.AbortMultipartUpload(opCtx, &awss3.AbortMultipartUploadInput{
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
