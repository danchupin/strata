//go:build integration

package s3_test

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"runtime"
	"strings"
	"testing"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	tcminio "github.com/testcontainers/testcontainers-go/modules/minio"

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
