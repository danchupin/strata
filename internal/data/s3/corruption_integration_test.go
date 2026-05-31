//go:build integration

package s3_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/testcontainers/testcontainers-go"
	tcminio "github.com/testcontainers/testcontainers-go/modules/minio"

	"github.com/danchupin/strata/internal/data"
	s3backend "github.com/danchupin/strata/internal/data/s3"
)

// TestGetChunksMissingBackendObjectFailsLoud pins the US-011 chunk-loss
// invariant for the S3/MinIO data plane: when the backing object behind a
// BackendRef manifest is gone (deleted out from under the gateway), a
// GetChunks for the full object range must fail loud with data.ErrNotFound —
// never return a reader that yields a clean, short, "successful" body that the
// gateway would serve as a silently-truncated 200.
//
// Builds on the established missing-key contract (b.GetRange on an absent key
// returns data.ErrNotFound) but exercises the manifest-level GetChunks path
// the GET handler actually drives, which the existing b.Get / b.GetRange
// missing-key tests do not cover. Runs only under `go test -tags integration`.
func TestGetChunksMissingBackendObjectFailsLoud(t *testing.T) {
	ctx := context.Background()

	const (
		username = "minioadmin"
		password = "minioadmin"
		bucket   = "strata-test-chunkloss"
	)

	container, err := tcminio.Run(ctx, "minio/minio:latest",
		tcminio.WithUsername(username),
		tcminio.WithPassword(password),
		testcontainers.WithEnv(map[string]string{"MINIO_KMS_SECRET_KEY": minioKMSSecretKey}),
	)
	if err != nil {
		t.Fatalf("start minio: %v", err)
	}
	t.Cleanup(func() {
		if err := container.Terminate(context.Background()); err != nil {
			t.Logf("terminate: %v", err)
		}
	})

	endpoint := minioEndpoint(t, ctx, container)
	if err := createBucket(ctx, endpoint, username, password, bucket); err != nil {
		t.Fatalf("create bucket: %v", err)
	}

	cfg := s3backend.Config{
		Endpoint:       endpoint,
		Region:         "us-east-1",
		Bucket:         bucket,
		AccessKey:      username,
		SecretKey:      password,
		ForcePathStyle: true,
	}
	b, err := s3backend.Open(ctx, cfg)
	if err != nil {
		t.Fatalf("open backend: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })

	body := bytes.Repeat([]byte("z"), 4096)
	bucketID := newUUID(t)
	m, err := b.PutChunks(data.WithBucketID(ctx, bucketID), bytes.NewReader(body), "STANDARD")
	if err != nil {
		t.Fatalf("PutChunks: %v", err)
	}
	if m.BackendRef == nil {
		t.Fatal("BackendRef nil after PutChunks")
	}

	// Sanity: the object reads back clean BEFORE we destroy it, so the
	// post-delete failure is the deletion, not a setup error.
	rc, err := b.GetChunks(ctx, m, 0, m.Size)
	if err != nil {
		t.Fatalf("GetChunks (pre-delete): %v", err)
	}
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("pre-delete round-trip mismatch: err=%v len=%d", err, len(got))
	}

	// Delete the backend object out from under the manifest.
	admin := newAdminClient(endpoint, username, password)
	if _, err := admin.DeleteObject(ctx, &awss3.DeleteObjectInput{
		Bucket: ptr(bucket),
		Key:    ptr(m.BackendRef.Key),
	}); err != nil {
		t.Fatalf("delete backend object: %v", err)
	}

	// GetChunks must now fail loud. ErrNotFound surfaces at open (GetChunks ->
	// GetRange). If it ever returned a usable reader, the gateway would write a
	// 200 with a body shorter than the manifest size — exactly the silent
	// truncation US-011 forbids.
	rc, err = b.GetChunks(ctx, m, 0, m.Size)
	if err == nil {
		leaked, rerr := io.ReadAll(rc)
		_ = rc.Close()
		t.Fatalf("GetChunks on a deleted backend object returned no error; read %d bytes (rerr=%v) — silent data loss", len(leaked), rerr)
	}
	if !errors.Is(err, data.ErrNotFound) {
		t.Fatalf("GetChunks (post-delete): want data.ErrNotFound, got %v", err)
	}
}
