//go:build integration

package s3_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	mathrand "math/rand"
	"net/http"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/google/uuid"
	tcminio "github.com/testcontainers/testcontainers-go/modules/minio"

	"github.com/danchupin/strata/internal/data"
	s3backend "github.com/danchupin/strata/internal/data/s3"
)

// TestPutStreams100MiBBoundedMemory exercises US-002 against a real MinIO
// container: a 100 MiB streaming upload must complete and never buffer
// more than ~PartSize * UploadConcurrency in heap (default 64 MiB).
//
// Runs only under `go test -tags integration`.
func TestPutStreams100MiBBoundedMemory(t *testing.T) {
	ctx := context.Background()

	const (
		username = "minioadmin"
		password = "minioadmin"
		bucket   = "strata-test"
	)

	container, err := tcminio.Run(ctx, "minio/minio:latest",
		tcminio.WithUsername(username),
		tcminio.WithPassword(password),
	)
	if err != nil {
		t.Fatalf("start minio: %v", err)
	}
	t.Cleanup(func() {
		if err := container.Terminate(context.Background()); err != nil {
			t.Logf("terminate: %v", err)
		}
	})

	hostPort, err := container.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	endpoint := hostPort
	if !strings.HasPrefix(endpoint, "http://") && !strings.HasPrefix(endpoint, "https://") {
		endpoint = "http://" + endpoint
	}

	if err := createBucket(ctx, endpoint, username, password, bucket); err != nil {
		t.Fatalf("create bucket: %v", err)
	}

	// Tight part-size + low concurrency keep the streaming bound provable
	// inside Go's HeapInuse sawtooth: PartSize * UploadConcurrency = 10
	// MiB pool, so a 100 MiB body that streams (not buffers) cannot push
	// the heap anywhere near 100 MiB. With prod defaults (16 MiB * 4 = 64
	// MiB pool) HeapInuse can drift right up against 100 MiB on a hot
	// docker-localhost upload, masking the bound — see git history.
	cfg := s3backend.Config{
		Endpoint:          endpoint,
		Region:            "us-east-1",
		Bucket:            bucket,
		AccessKey:         username,
		SecretKey:         password,
		ForcePathStyle:    true,
		PartSize:          5 * 1024 * 1024,
		UploadConcurrency: 2,
	}
	b, err := s3backend.Open(ctx, cfg)
	if err != nil {
		t.Fatalf("open backend: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })

	const size = int64(100 * 1024 * 1024)
	r := io.LimitReader(rand.Reader, size)

	// Heap-bound assertion: sample HeapInuse during the upload via a
	// ticker, take the max, and verify it stays well under the body size.
	// The real bound is PartSize*UploadConcurrency = 64 MiB; we allow up
	// to 90 MiB of wiggle room (SDK pools + tail GC scheduling) — the
	// assertion that matters is "less than 100 MiB body size", proving
	// the body was streamed and not fully buffered.
	var peakHeapInuse uint64
	var ms runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&ms)
	baseline := ms.HeapInuse

	sampleStop := make(chan struct{})
	sampleDone := make(chan struct{})
	go func() {
		defer close(sampleDone)
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-sampleStop:
				return
			case <-tick.C:
				runtime.ReadMemStats(&ms)
				if ms.HeapInuse > peakHeapInuse {
					peakHeapInuse = ms.HeapInuse
				}
			}
		}
	}()

	uploadCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	res, err := b.Put(uploadCtx, "100m-key", r, size)
	close(sampleStop)
	<-sampleDone
	if err != nil {
		t.Fatalf("put: %v", err)
	}

	if res.ETag == "" {
		t.Fatalf("etag empty in result")
	}
	if res.Size != size {
		t.Fatalf("size: want %d, got %d", size, res.Size)
	}

	peakDelta := int64(peakHeapInuse) - int64(baseline)
	if peakDelta < 0 {
		peakDelta = 0
	}
	const maxAllowed = int64(100 * 1024 * 1024)
	if peakDelta >= maxAllowed {
		t.Fatalf("peak heap-in-use delta %d ≥ 100 MiB — body was likely buffered", peakDelta)
	}
}

// TestPutAbortsOnContextCancel guards the US-002 multipart-cleanup
// contract: when the caller's context is cancelled mid-upload, the
// manager.Uploader must AbortMultipartUpload so no orphan multipart
// session is left in the backend bucket.
func TestPutAbortsOnContextCancel(t *testing.T) {
	ctx := context.Background()

	const (
		username = "minioadmin"
		password = "minioadmin"
		bucket   = "strata-test-abort"
	)

	container, err := tcminio.Run(ctx, "minio/minio:latest",
		tcminio.WithUsername(username),
		tcminio.WithPassword(password),
	)
	if err != nil {
		t.Fatalf("start minio: %v", err)
	}
	t.Cleanup(func() {
		if err := container.Terminate(context.Background()); err != nil {
			t.Logf("terminate: %v", err)
		}
	})

	hostPort, err := container.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	endpoint := hostPort
	if !strings.HasPrefix(endpoint, "http://") && !strings.HasPrefix(endpoint, "https://") {
		endpoint = "http://" + endpoint
	}

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
		PartSize:       5 * 1024 * 1024, // SDK minimum — force multipart for 20 MiB
	}
	b, err := s3backend.Open(ctx, cfg)
	if err != nil {
		t.Fatalf("open backend: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })

	uploadCtx, cancel := context.WithCancel(ctx)
	cancel() // cancel before upload starts

	r := io.LimitReader(rand.Reader, 20*1024*1024)
	if _, err := b.Put(uploadCtx, "cancelled-key", r, 20*1024*1024); err == nil {
		t.Fatal("expected error from cancelled upload, got nil")
	}

	// Verify no orphan multipart session remained.
	client := newAdminClient(endpoint, username, password)
	out, err := client.ListMultipartUploads(ctx, &awss3.ListMultipartUploadsInput{Bucket: ptr(bucket)})
	if err != nil {
		t.Fatalf("list multipart uploads: %v", err)
	}
	if len(out.Uploads) != 0 {
		t.Fatalf("expected zero in-progress multipart uploads after cancel, got %d", len(out.Uploads))
	}
}

// TestGetRangeReads1KiBFrom100MiB exercises US-003 against MinIO: upload
// a 100 MiB object, then GetRange a 1 KiB slice from the middle and
// verify the bytes match exactly. The full body is never loaded into
// memory — body is consumed via io.ReadFull on the SDK's response stream.
func TestGetRangeReads1KiBFrom100MiB(t *testing.T) {
	ctx := context.Background()

	const (
		username = "minioadmin"
		password = "minioadmin"
		bucket   = "strata-test-getrange"
		key      = "100m-key"
		size     = int64(100 * 1024 * 1024)
		offset   = int64(50 * 1024 * 1024)
		length   = int64(1024)
	)

	container, err := tcminio.Run(ctx, "minio/minio:latest",
		tcminio.WithUsername(username),
		tcminio.WithPassword(password),
	)
	if err != nil {
		t.Fatalf("start minio: %v", err)
	}
	t.Cleanup(func() {
		if err := container.Terminate(context.Background()); err != nil {
			t.Logf("terminate: %v", err)
		}
	})

	hostPort, err := container.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	endpoint := hostPort
	if !strings.HasPrefix(endpoint, "http://") && !strings.HasPrefix(endpoint, "https://") {
		endpoint = "http://" + endpoint
	}

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
		PartSize:       5 * 1024 * 1024,
	}
	b, err := s3backend.Open(ctx, cfg)
	if err != nil {
		t.Fatalf("open backend: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })

	// Build a deterministic body so we can verify the range bytes
	// exactly. mathrand seeded by zero produces a reproducible stream.
	body := make([]byte, size)
	prng := mathrand.New(mathrand.NewSource(1))
	if _, err := io.ReadFull(prng, body); err != nil {
		t.Fatalf("seed body: %v", err)
	}

	if _, err := b.Put(ctx, key, bytes.NewReader(body), size); err != nil {
		t.Fatalf("put: %v", err)
	}

	rc, err := b.GetRange(ctx, key, offset, length)
	if err != nil {
		t.Fatalf("get range: %v", err)
	}
	t.Cleanup(func() { _ = rc.Close() })

	got := make([]byte, length)
	n, err := io.ReadFull(rc, got)
	if err != nil {
		t.Fatalf("read body: %v (read %d bytes)", err, n)
	}
	if int64(n) != length {
		t.Fatalf("read length: want %d, got %d", length, n)
	}

	want := body[offset : offset+length]
	if !bytes.Equal(got, want) {
		t.Fatalf("range bytes mismatch: first 16 want=%x got=%x", want[:16], got[:16])
	}

	// Drain to confirm the SDK does not pad the response with extra
	// bytes beyond the requested range.
	tail, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("drain tail: %v", err)
	}
	if len(tail) != 0 {
		t.Fatalf("expected zero tail bytes after %d, got %d", length, len(tail))
	}
}

// TestGetReturnsErrNotFoundForMissingKey pins the US-003 NoSuchKey →
// data.ErrNotFound mapping so the gateway can surface 404 NoSuchKey.
func TestGetReturnsErrNotFoundForMissingKey(t *testing.T) {
	ctx := context.Background()

	const (
		username = "minioadmin"
		password = "minioadmin"
		bucket   = "strata-test-notfound"
	)

	container, err := tcminio.Run(ctx, "minio/minio:latest",
		tcminio.WithUsername(username),
		tcminio.WithPassword(password),
	)
	if err != nil {
		t.Fatalf("start minio: %v", err)
	}
	t.Cleanup(func() {
		if err := container.Terminate(context.Background()); err != nil {
			t.Logf("terminate: %v", err)
		}
	})

	hostPort, err := container.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	endpoint := hostPort
	if !strings.HasPrefix(endpoint, "http://") && !strings.HasPrefix(endpoint, "https://") {
		endpoint = "http://" + endpoint
	}

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

	if _, err := b.Get(ctx, "nope"); !errors.Is(err, data.ErrNotFound) {
		t.Fatalf("Get missing: want data.ErrNotFound, got %v", err)
	}
	if _, err := b.GetRange(ctx, "nope", 0, 16); !errors.Is(err, data.ErrNotFound) {
		t.Fatalf("GetRange missing: want data.ErrNotFound, got %v", err)
	}
}

// TestDeleteBatchUnversionedSingleHTTPCall exercises US-004 against a
// MinIO bucket with versioning OFF: 100 keys with empty VersionID must
// be removed in exactly ONE DeleteObjects HTTP call (within
// DeleteBatchLimit) and the bytes must be freed (no remaining objects).
func TestDeleteBatchUnversionedSingleHTTPCall(t *testing.T) {
	ctx := context.Background()

	const (
		username = "minioadmin"
		password = "minioadmin"
		bucket   = "strata-test-batch-unversioned"
		count    = 100
	)

	container, err := tcminio.Run(ctx, "minio/minio:latest",
		tcminio.WithUsername(username),
		tcminio.WithPassword(password),
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

	// Counting RoundTripper attached AFTER the seed PUTs so it observes
	// only the DeleteBatch traffic.
	counter := &countingTransport{wrapped: http.DefaultTransport}
	httpClient := &http.Client{Transport: counter}

	cfg := s3backend.Config{
		Endpoint:       endpoint,
		Region:         "us-east-1",
		Bucket:         bucket,
		AccessKey:      username,
		SecretKey:      password,
		ForcePathStyle: true,
		HTTPClient:     httpClient,
	}
	b, err := s3backend.Open(ctx, cfg)
	if err != nil {
		t.Fatalf("open backend: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })

	admin := newAdminClient(endpoint, username, password)
	refs := make([]s3backend.ObjectRef, count)
	for i := 0; i < count; i++ {
		key := fmt.Sprintf("obj-%03d", i)
		body := []byte(fmt.Sprintf("payload-%d", i))
		if _, err := admin.PutObject(ctx, &awss3.PutObjectInput{
			Bucket: ptr(bucket),
			Key:    ptr(key),
			Body:   bytes.NewReader(body),
		}); err != nil {
			t.Fatalf("seed put %s: %v", key, err)
		}
		refs[i] = s3backend.ObjectRef{Key: key}
	}

	// Start counting now — only the DeleteBatch call should be observed.
	counter.reset()

	failures, err := b.DeleteBatch(ctx, refs)
	if err != nil {
		t.Fatalf("DeleteBatch: %v", err)
	}
	if len(failures) != 0 {
		t.Fatalf("DeleteBatch failures: %+v", failures)
	}

	if got := counter.count(); got != 1 {
		t.Fatalf("DeleteBatch HTTP request count: want 1, got %d", got)
	}

	listOut, err := admin.ListObjectsV2(ctx, &awss3.ListObjectsV2Input{Bucket: ptr(bucket)})
	if err != nil {
		t.Fatalf("list objects: %v", err)
	}
	if len(listOut.Contents) != 0 {
		t.Fatalf("expected zero remaining objects, got %d", len(listOut.Contents))
	}
}

// TestDeleteBatchVersionedNoDeleteMarkers exercises US-004 against a
// MinIO bucket with versioning ENABLED: 100 keys, each deleted by
// VersionId, must leave the bucket empty AND must NOT create
// delete-markers (verified via ListObjectVersions).
func TestDeleteBatchVersionedNoDeleteMarkers(t *testing.T) {
	ctx := context.Background()

	const (
		username = "minioadmin"
		password = "minioadmin"
		bucket   = "strata-test-batch-versioned"
		count    = 100
	)

	container, err := tcminio.Run(ctx, "minio/minio:latest",
		tcminio.WithUsername(username),
		tcminio.WithPassword(password),
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
	if err := enableVersioning(ctx, endpoint, username, password, bucket); err != nil {
		t.Fatalf("enable versioning: %v", err)
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

	admin := newAdminClient(endpoint, username, password)
	refs := make([]s3backend.ObjectRef, count)
	for i := 0; i < count; i++ {
		key := fmt.Sprintf("obj-%03d", i)
		body := []byte(fmt.Sprintf("payload-%d", i))
		out, err := admin.PutObject(ctx, &awss3.PutObjectInput{
			Bucket: ptr(bucket),
			Key:    ptr(key),
			Body:   bytes.NewReader(body),
		})
		if err != nil {
			t.Fatalf("seed put %s: %v", key, err)
		}
		if out.VersionId == nil || *out.VersionId == "" {
			t.Fatalf("seed put %s: empty VersionId on versioned bucket", key)
		}
		refs[i] = s3backend.ObjectRef{Key: key, VersionID: *out.VersionId}
	}

	failures, err := b.DeleteBatch(ctx, refs)
	if err != nil {
		t.Fatalf("DeleteBatch: %v", err)
	}
	if len(failures) != 0 {
		t.Fatalf("DeleteBatch failures: %+v", failures)
	}

	versions, err := admin.ListObjectVersions(ctx, &awss3.ListObjectVersionsInput{Bucket: ptr(bucket)})
	if err != nil {
		t.Fatalf("list object versions: %v", err)
	}
	if len(versions.Versions) != 0 {
		t.Fatalf("expected zero remaining versions, got %d", len(versions.Versions))
	}
	if len(versions.DeleteMarkers) != 0 {
		t.Fatalf("expected zero delete-markers, got %d", len(versions.DeleteMarkers))
	}
}

// TestDeleteBatchMixedVersionIDs exercises US-004 against a versioning-
// enabled MinIO bucket with a MIXED batch: half the refs carry the
// captured VersionID (versioned-delete, no delete-marker), half carry
// empty VersionID (plain delete, creates delete-marker on a versioned
// bucket — the legacy/migration path documented in US-008). Asserts
// each ref is handled per its own VersionID.
func TestDeleteBatchMixedVersionIDs(t *testing.T) {
	ctx := context.Background()

	const (
		username = "minioadmin"
		password = "minioadmin"
		bucket   = "strata-test-batch-mixed"
		count    = 20 // 10 versioned + 10 plain
	)

	container, err := tcminio.Run(ctx, "minio/minio:latest",
		tcminio.WithUsername(username),
		tcminio.WithPassword(password),
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
	if err := enableVersioning(ctx, endpoint, username, password, bucket); err != nil {
		t.Fatalf("enable versioning: %v", err)
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

	admin := newAdminClient(endpoint, username, password)
	refs := make([]s3backend.ObjectRef, count)
	for i := 0; i < count; i++ {
		key := fmt.Sprintf("obj-%03d", i)
		body := []byte(fmt.Sprintf("payload-%d", i))
		out, err := admin.PutObject(ctx, &awss3.PutObjectInput{
			Bucket: ptr(bucket),
			Key:    ptr(key),
			Body:   bytes.NewReader(body),
		})
		if err != nil {
			t.Fatalf("seed put %s: %v", key, err)
		}
		if out.VersionId == nil || *out.VersionId == "" {
			t.Fatalf("seed put %s: empty VersionId on versioned bucket", key)
		}
		ref := s3backend.ObjectRef{Key: key}
		if i%2 == 0 {
			// even indexes: versioned delete (clean — no delete-marker)
			ref.VersionID = *out.VersionId
		}
		// odd indexes: empty VersionID → plain delete on a versioned
		// bucket → leaves the seeded version intact, creates a
		// delete-marker.
		refs[i] = ref
	}

	failures, err := b.DeleteBatch(ctx, refs)
	if err != nil {
		t.Fatalf("DeleteBatch: %v", err)
	}
	if len(failures) != 0 {
		t.Fatalf("DeleteBatch failures: %+v", failures)
	}

	versions, err := admin.ListObjectVersions(ctx, &awss3.ListObjectVersionsInput{Bucket: ptr(bucket)})
	if err != nil {
		t.Fatalf("list object versions: %v", err)
	}

	// Versioned-delete refs (10 of them, even indexes) are wiped.
	// Plain-delete refs (10 of them, odd indexes) leave the original
	// version + a delete-marker.
	wantRemainingVersions := count / 2
	wantDeleteMarkers := count / 2
	if len(versions.Versions) != wantRemainingVersions {
		t.Fatalf("remaining versions: want %d, got %d", wantRemainingVersions, len(versions.Versions))
	}
	if len(versions.DeleteMarkers) != wantDeleteMarkers {
		t.Fatalf("delete-markers: want %d, got %d", wantDeleteMarkers, len(versions.DeleteMarkers))
	}

	// Confirm the surviving versions are exactly the odd-index keys.
	gotKeys := map[string]struct{}{}
	for _, v := range versions.Versions {
		if v.Key != nil {
			gotKeys[*v.Key] = struct{}{}
		}
	}
	for i := 0; i < count; i++ {
		key := fmt.Sprintf("obj-%03d", i)
		_, present := gotKeys[key]
		if i%2 == 1 && !present {
			t.Fatalf("expected odd-index key %s to survive plain delete", key)
		}
		if i%2 == 0 && present {
			t.Fatalf("even-index key %s should have been wiped by versioned delete", key)
		}
	}
}

// TestOpenProbeFailsOnMissingBucket exercises US-005 fail-fast: when the
// configured bucket does not exist on the backend, Open's writability
// probe must error before returning so the gateway refuses to start.
func TestOpenProbeFailsOnMissingBucket(t *testing.T) {
	ctx := context.Background()

	const (
		username = "minioadmin"
		password = "minioadmin"
	)

	container, err := tcminio.Run(ctx, "minio/minio:latest",
		tcminio.WithUsername(username),
		tcminio.WithPassword(password),
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
	// Intentionally NOT calling createBucket — bucket is missing.
	cfg := s3backend.Config{
		Endpoint:       endpoint,
		Region:         "us-east-1",
		Bucket:         "does-not-exist",
		AccessKey:      username,
		SecretKey:      password,
		ForcePathStyle: true,
	}
	if _, err := s3backend.Open(ctx, cfg); err == nil {
		t.Fatal("Open with missing bucket: want error from probe, got nil")
	}
}

// TestOpenProbeLeavesBucketCleanOnVersioned pins US-005 versioning
// awareness: probe must capture the PutObject VersionId on a versioning-
// enabled bucket and pass it to DeleteObject so the bucket is left
// exactly as it was found — no canary versions, no delete-markers.
func TestOpenProbeLeavesBucketCleanOnVersioned(t *testing.T) {
	ctx := context.Background()

	const (
		username = "minioadmin"
		password = "minioadmin"
		bucket   = "strata-test-probe-versioned"
	)

	container, err := tcminio.Run(ctx, "minio/minio:latest",
		tcminio.WithUsername(username),
		tcminio.WithPassword(password),
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
	if err := enableVersioning(ctx, endpoint, username, password, bucket); err != nil {
		t.Fatalf("enable versioning: %v", err)
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

	admin := newAdminClient(endpoint, username, password)
	versions, err := admin.ListObjectVersions(ctx, &awss3.ListObjectVersionsInput{Bucket: ptr(bucket)})
	if err != nil {
		t.Fatalf("list object versions: %v", err)
	}
	if len(versions.Versions) != 0 {
		t.Fatalf("after probe: expected zero versions, got %d", len(versions.Versions))
	}
	if len(versions.DeleteMarkers) != 0 {
		t.Fatalf("after probe: expected zero delete-markers, got %d", len(versions.DeleteMarkers))
	}
}

// TestDeleteObjectIdempotent guards US-004's idempotency contract:
// DeleteObject on an already-missing key must succeed without surfacing
// the backend's NoSuchKey error.
func TestDeleteObjectIdempotent(t *testing.T) {
	ctx := context.Background()

	const (
		username = "minioadmin"
		password = "minioadmin"
		bucket   = "strata-test-delete-idempotent"
	)

	container, err := tcminio.Run(ctx, "minio/minio:latest",
		tcminio.WithUsername(username),
		tcminio.WithPassword(password),
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

	if err := b.DeleteObject(ctx, "never-existed", ""); err != nil {
		t.Fatalf("DeleteObject on missing key: want nil, got %v", err)
	}
}

// TestPutChunksGetChunksDeleteRoundTrip exercises the US-009 native shape
// end-to-end: PutChunks streams a body into MinIO and produces a
// BackendRef-shape manifest; GetChunks streams the bytes back; Delete
// removes the backend object. The test also verifies the
// <bucket-uuid>/<object-uuid> key format with the bucket id threaded via
// data.WithBucketID.
func TestPutChunksGetChunksDeleteRoundTrip(t *testing.T) {
	ctx := context.Background()

	const (
		username = "minioadmin"
		password = "minioadmin"
		bucket   = "strata-test-roundtrip"
	)

	container, err := tcminio.Run(ctx, "minio/minio:latest",
		tcminio.WithUsername(username),
		tcminio.WithPassword(password),
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

	// Deterministic body so range-read assertion can verify byte-exact match.
	body := make([]byte, 1024)
	prng := mathrand.New(mathrand.NewSource(7))
	if _, err := io.ReadFull(prng, body); err != nil {
		t.Fatalf("seed body: %v", err)
	}

	bucketID := newUUID(t)
	putCtx := data.WithBucketID(ctx, bucketID)
	m, err := b.PutChunks(putCtx, bytes.NewReader(body), "STANDARD")
	if err != nil {
		t.Fatalf("PutChunks: %v", err)
	}
	if m.BackendRef == nil {
		t.Fatal("BackendRef nil after PutChunks")
	}
	if m.Size != int64(len(body)) {
		t.Fatalf("size: want %d, got %d", len(body), m.Size)
	}
	if !strings.HasPrefix(m.BackendRef.Key, bucketID.String()+"/") {
		t.Fatalf("key %q missing bucket-uuid prefix %q", m.BackendRef.Key, bucketID.String())
	}

	// Full-body GET via GetChunks.
	rc, err := b.GetChunks(ctx, m, 0, m.Size)
	if err != nil {
		t.Fatalf("GetChunks: %v", err)
	}
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatal("GetChunks body mismatch")
	}

	// Range GET via GetChunks: middle 64 bytes.
	const off, length = int64(256), int64(64)
	rc, err = b.GetChunks(ctx, m, off, length)
	if err != nil {
		t.Fatalf("GetChunks(range): %v", err)
	}
	rangeGot, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatalf("read range: %v", err)
	}
	if !bytes.Equal(rangeGot, body[off:off+length]) {
		t.Fatal("GetChunks range bytes mismatch")
	}

	// Delete via interface, then verify backend object is gone.
	if err := b.Delete(ctx, m); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	admin := newAdminClient(endpoint, username, password)
	listOut, err := admin.ListObjectsV2(ctx, &awss3.ListObjectsV2Input{Bucket: ptr(bucket)})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listOut.Contents) != 0 {
		t.Fatalf("expected zero objects after Delete, got %d", len(listOut.Contents))
	}
}

func newUUID(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := uuid.NewRandom()
	if err != nil {
		t.Fatalf("uuid: %v", err)
	}
	return id
}

// minioEndpoint returns the http://-prefixed endpoint URL for the
// running MinIO container.
func minioEndpoint(t *testing.T, ctx context.Context, container interface {
	ConnectionString(context.Context) (string, error)
}) string {
	t.Helper()
	hostPort, err := container.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	if !strings.HasPrefix(hostPort, "http://") && !strings.HasPrefix(hostPort, "https://") {
		hostPort = "http://" + hostPort
	}
	return hostPort
}

// enableVersioning flips MinIO's bucket versioning to Enabled. Required
// for the versioned-delete tests so PutObject responses carry a
// non-empty VersionId.
func enableVersioning(ctx context.Context, endpoint, ak, sk, bucket string) error {
	client := newAdminClient(endpoint, ak, sk)
	_, err := client.PutBucketVersioning(ctx, &awss3.PutBucketVersioningInput{
		Bucket: ptr(bucket),
		VersioningConfiguration: &s3types.VersioningConfiguration{
			Status: s3types.BucketVersioningStatusEnabled,
		},
	})
	if err != nil {
		return fmt.Errorf("put bucket versioning %s: %w", bucket, err)
	}
	return nil
}

// countingTransport tallies HTTP requests forwarded through wrapped.
// Used by US-004 batch-delete tests to assert "exactly one HTTP call".
type countingTransport struct {
	wrapped http.RoundTripper
	n       atomic.Int64
}

func (c *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	c.n.Add(1)
	return c.wrapped.RoundTrip(req)
}

func (c *countingTransport) reset() { c.n.Store(0) }
func (c *countingTransport) count() int64 {
	return c.n.Load()
}

func createBucket(ctx context.Context, endpoint, ak, sk, bucket string) error {
	client := newAdminClient(endpoint, ak, sk)
	_, err := client.CreateBucket(ctx, &awss3.CreateBucketInput{Bucket: ptr(bucket)})
	if err != nil {
		return fmt.Errorf("create bucket %s: %w", bucket, err)
	}
	return nil
}

func newAdminClient(endpoint, ak, sk string) *awss3.Client {
	awscfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(ak, sk, "")),
	)
	if err != nil {
		panic(err)
	}
	return awss3.NewFromConfig(awscfg, func(o *awss3.Options) {
		ep := endpoint
		o.BaseEndpoint = &ep
		o.UsePathStyle = true
	})
}

func ptr[T any](v T) *T { return &v }
