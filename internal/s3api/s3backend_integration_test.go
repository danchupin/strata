//go:build integration

package s3api_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	mathrand "math/rand"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	tcminio "github.com/testcontainers/testcontainers-go/modules/minio"

	datas3 "github.com/danchupin/strata/internal/data/s3"
	metamem "github.com/danchupin/strata/internal/meta/memory"
	"github.com/danchupin/strata/internal/s3api"
)

// TestGatewayDispatchesToS3Backend exercises US-009 end-to-end: a fully
// wired Server (s3api.New) with the s3-over-s3 data backend Open()'d
// against a real MinIO container. PUT/GET/DELETE through the HTTP path
// must produce a backend object on each PUT, serve range reads, and
// remove the backend object on DELETE — proving the dispatch + manifest
// BackendRef + GetRange path work without code changes elsewhere in the
// gateway.
func TestGatewayDispatchesToS3Backend(t *testing.T) {
	ctx := context.Background()

	const (
		username = "minioadmin"
		password = "minioadmin"
		bucket   = "strata-gateway-s3"
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

	admin := newRawAdminClient(endpoint, username, password)
	if _, err := admin.CreateBucket(ctx, &awss3.CreateBucketInput{Bucket: ptrString(bucket)}); err != nil {
		t.Fatalf("create backend bucket: %v", err)
	}

	backend, err := datas3.Open(ctx, datas3.Config{
		Endpoint:       endpoint,
		Region:         "us-east-1",
		Bucket:         bucket,
		AccessKey:      username,
		SecretKey:      password,
		ForcePathStyle: true,
	})
	if err != nil {
		t.Fatalf("open s3 backend: %v", err)
	}
	t.Cleanup(func() { _ = backend.Close() })

	api := s3api.New(backend, metamem.New())
	api.Region = "default"
	ts := httptest.NewServer(api)
	t.Cleanup(ts.Close)

	// Seed a Strata bucket.
	must(t, http.MethodPut, ts.URL+"/strata-bkt", nil, http.StatusOK)

	// PUT a small object → assert exactly one backend object lands.
	body := []byte("hello strata via s3 backend")
	must(t, http.MethodPut, ts.URL+"/strata-bkt/k", bytes.NewReader(body), http.StatusOK)
	listOut, err := admin.ListObjectsV2(ctx, &awss3.ListObjectsV2Input{Bucket: ptrString(bucket)})
	if err != nil {
		t.Fatalf("list backend: %v", err)
	}
	if len(listOut.Contents) != 1 {
		t.Fatalf("expected 1 backend object after small PUT, got %d", len(listOut.Contents))
	}

	// GET round-trips the bytes.
	resp, err := http.Get(ts.URL + "/strata-bkt/k")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status: %d", resp.StatusCode)
	}
	got, err := readAllAndClose(resp)
	if err != nil {
		t.Fatalf("GET body: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("GET body mismatch: %q vs %q", got, body)
	}

	// PUT a 5 MiB object so the manager.Uploader exercises multipart;
	// gateway must still surface exactly one backend object per Strata
	// object (US-009 native shape — never N chunks-as-S3-objects).
	big := make([]byte, 5*1024*1024)
	prng := mathrand.New(mathrand.NewSource(99))
	if _, err := prng.Read(big); err != nil {
		t.Fatalf("seed big: %v", err)
	}
	must(t, http.MethodPut, ts.URL+"/strata-bkt/big.bin", bytes.NewReader(big), http.StatusOK)

	// Range GET into the 5 MiB blob through the gateway.
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/strata-bkt/big.bin", nil)
	if err != nil {
		t.Fatalf("range req: %v", err)
	}
	req.Header.Set("Range", "bytes=1024-2047")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("range GET: %v", err)
	}
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("range GET status: %d", resp.StatusCode)
	}
	rng, err := readAllAndClose(resp)
	if err != nil {
		t.Fatalf("range GET body: %v", err)
	}
	if !bytes.Equal(rng, big[1024:2048]) {
		t.Fatalf("range bytes mismatch")
	}

	// DELETE small object → the backend object behind it goes away.
	must(t, http.MethodDelete, ts.URL+"/strata-bkt/k", nil, http.StatusNoContent)
	listOut, err = admin.ListObjectsV2(ctx, &awss3.ListObjectsV2Input{Bucket: ptrString(bucket)})
	if err != nil {
		t.Fatalf("list backend after delete: %v", err)
	}
	if len(listOut.Contents) != 1 {
		t.Fatalf("expected 1 backend object after small DELETE (only big remains), got %d", len(listOut.Contents))
	}
}

func must(t *testing.T, method, url string, body *bytes.Reader, want int) {
	t.Helper()
	var req *http.Request
	var err error
	if body == nil {
		req, err = http.NewRequest(method, url, nil)
	} else {
		req, err = http.NewRequest(method, url, body)
	}
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != want {
		t.Fatalf("%s %s status: got %d want %d", method, url, resp.StatusCode, want)
	}
}

func readAllAndClose(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, err := buf.ReadFrom(resp.Body)
	return buf.Bytes(), err
}

var initiateUploadIDRE = regexp.MustCompile(`<UploadId>([^<]+)</UploadId>`)

// TestGatewayMultipartPassThroughToS3Backend exercises US-010 end-to-end:
// 5 client UploadParts (each 5 MiB, total 25 MiB so MinIO accepts the
// minimum part size) flow through Strata's multipart endpoint and map 1:1
// onto a single backend multipart upload. After CompleteMultipartUpload
// the backend bucket contains exactly one object (the assembled native-
// shape result), not five — proving the pass-through replaces the
// chunks-per-object code path.
func TestGatewayMultipartPassThroughToS3Backend(t *testing.T) {
	ctx := context.Background()

	const (
		username = "minioadmin"
		password = "minioadmin"
		bucket   = "strata-mp-passthrough"
		partSize = 10 * 1024 * 1024 // AC US-010: 50 MiB across 5 parts.
		nparts   = 5
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

	admin := newRawAdminClient(endpoint, username, password)
	if _, err := admin.CreateBucket(ctx, &awss3.CreateBucketInput{Bucket: ptrString(bucket)}); err != nil {
		t.Fatalf("create backend bucket: %v", err)
	}

	backend, err := datas3.Open(ctx, datas3.Config{
		Endpoint:       endpoint,
		Region:         "us-east-1",
		Bucket:         bucket,
		AccessKey:      username,
		SecretKey:      password,
		ForcePathStyle: true,
	})
	if err != nil {
		t.Fatalf("open s3 backend: %v", err)
	}
	t.Cleanup(func() { _ = backend.Close() })

	api := s3api.New(backend, metamem.New())
	api.Region = "default"
	ts := httptest.NewServer(api)
	t.Cleanup(ts.Close)

	must(t, http.MethodPut, ts.URL+"/strata-bkt", nil, http.StatusOK)

	// Initiate.
	resp, err := http.DefaultClient.Do(mustReq(t, http.MethodPost, ts.URL+"/strata-bkt/big-mp?uploads", nil))
	if err != nil {
		t.Fatalf("initiate: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("initiate status: %d", resp.StatusCode)
	}
	body, err := readAllAndClose(resp)
	if err != nil {
		t.Fatalf("initiate body: %v", err)
	}
	m := initiateUploadIDRE.FindStringSubmatch(string(body))
	if len(m) != 2 {
		t.Fatalf("no UploadId in initiate response: %s", body)
	}
	uploadID := m[1]

	// Upload N parts, each filled with deterministic bytes for byte-equal
	// readback assertion.
	prng := mathrand.New(mathrand.NewSource(42))
	var orig bytes.Buffer
	var completeBody strings.Builder
	completeBody.WriteString("<CompleteMultipartUpload>")
	for i := 1; i <= nparts; i++ {
		part := make([]byte, partSize)
		if _, err := io.ReadFull(prng, part); err != nil {
			t.Fatalf("seed part %d: %v", i, err)
		}
		orig.Write(part)
		url := fmt.Sprintf("%s/strata-bkt/big-mp?uploadId=%s&partNumber=%d", ts.URL, uploadID, i)
		req := mustReq(t, http.MethodPut, url, bytes.NewReader(part))
		req.ContentLength = int64(len(part))
		presp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("upload part %d: %v", i, err)
		}
		if presp.StatusCode != http.StatusOK {
			defer presp.Body.Close()
			b, _ := io.ReadAll(presp.Body)
			t.Fatalf("upload part %d status %d: %s", i, presp.StatusCode, b)
		}
		etag := strings.Trim(presp.Header.Get("Etag"), `"`)
		_ = presp.Body.Close()
		if etag == "" {
			t.Fatalf("upload part %d: empty etag", i)
		}
		completeBody.WriteString(fmt.Sprintf(`<Part><PartNumber>%d</PartNumber><ETag>"%s"</ETag></Part>`, i, etag))
	}
	completeBody.WriteString("</CompleteMultipartUpload>")

	// Complete.
	completeURL := fmt.Sprintf("%s/strata-bkt/big-mp?uploadId=%s", ts.URL, uploadID)
	cresp, err := http.DefaultClient.Do(mustReq(t, http.MethodPost, completeURL, strings.NewReader(completeBody.String())))
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if cresp.StatusCode != http.StatusOK {
		defer cresp.Body.Close()
		b, _ := io.ReadAll(cresp.Body)
		t.Fatalf("complete status %d: %s", cresp.StatusCode, b)
	}
	_ = cresp.Body.Close()

	// AC: exactly ONE backend object after multipart complete (1:1
	// invariant — never N parts-as-objects).
	listOut, err := admin.ListObjectsV2(ctx, &awss3.ListObjectsV2Input{Bucket: ptrString(bucket)})
	if err != nil {
		t.Fatalf("list backend: %v", err)
	}
	if len(listOut.Contents) != 1 {
		t.Fatalf("expected 1 backend object after multipart complete, got %d", len(listOut.Contents))
	}

	// Round-trip GET asserts the assembled object matches the input bytes.
	getResp, err := http.Get(ts.URL + "/strata-bkt/big-mp")
	if err != nil {
		t.Fatalf("GET assembled: %v", err)
	}
	got, err := readAllAndClose(getResp)
	if err != nil {
		t.Fatalf("GET body: %v", err)
	}
	if !bytes.Equal(got, orig.Bytes()) {
		t.Fatalf("assembled body mismatch: got %d bytes, want %d", len(got), orig.Len())
	}

	// DELETE removes the backend object too (orphan cleanup respects
	// BackendRef).
	must(t, http.MethodDelete, ts.URL+"/strata-bkt/big-mp", nil, http.StatusNoContent)
	listOut, err = admin.ListObjectsV2(ctx, &awss3.ListObjectsV2Input{Bucket: ptrString(bucket)})
	if err != nil {
		t.Fatalf("list backend after delete: %v", err)
	}
	if len(listOut.Contents) != 0 {
		t.Fatalf("expected 0 backend objects after DELETE, got %d", len(listOut.Contents))
	}
}

// TestGatewayMultipartAbortPassThrough exercises the US-010 abort path:
// after AbortMultipartUpload the backend session must be cancelled too —
// a subsequent ListMultipartUploads on the backend bucket returns empty.
func TestGatewayMultipartAbortPassThrough(t *testing.T) {
	ctx := context.Background()

	const (
		username = "minioadmin"
		password = "minioadmin"
		bucket   = "strata-mp-abort"
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

	admin := newRawAdminClient(endpoint, username, password)
	if _, err := admin.CreateBucket(ctx, &awss3.CreateBucketInput{Bucket: ptrString(bucket)}); err != nil {
		t.Fatalf("create backend bucket: %v", err)
	}

	backend, err := datas3.Open(ctx, datas3.Config{
		Endpoint:       endpoint,
		Region:         "us-east-1",
		Bucket:         bucket,
		AccessKey:      username,
		SecretKey:      password,
		ForcePathStyle: true,
	})
	if err != nil {
		t.Fatalf("open s3 backend: %v", err)
	}
	t.Cleanup(func() { _ = backend.Close() })

	api := s3api.New(backend, metamem.New())
	ts := httptest.NewServer(api)
	t.Cleanup(ts.Close)

	must(t, http.MethodPut, ts.URL+"/strata-bkt", nil, http.StatusOK)
	resp, err := http.DefaultClient.Do(mustReq(t, http.MethodPost, ts.URL+"/strata-bkt/abort-mp?uploads", nil))
	if err != nil {
		t.Fatalf("initiate: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("initiate status: %d", resp.StatusCode)
	}
	body, err := readAllAndClose(resp)
	if err != nil {
		t.Fatalf("initiate body: %v", err)
	}
	m := initiateUploadIDRE.FindStringSubmatch(string(body))
	if len(m) != 2 {
		t.Fatalf("no UploadId: %s", body)
	}
	uploadID := m[1]

	// Backend session is created by initiate — confirm.
	mpList, err := admin.ListMultipartUploads(ctx, &awss3.ListMultipartUploadsInput{Bucket: ptrString(bucket)})
	if err != nil {
		t.Fatalf("list multipart pre-abort: %v", err)
	}
	if len(mpList.Uploads) != 1 {
		t.Fatalf("expected 1 in-progress backend multipart, got %d", len(mpList.Uploads))
	}

	must(t, http.MethodDelete, fmt.Sprintf("%s/strata-bkt/abort-mp?uploadId=%s", ts.URL, uploadID), nil, http.StatusNoContent)

	mpList, err = admin.ListMultipartUploads(ctx, &awss3.ListMultipartUploadsInput{Bucket: ptrString(bucket)})
	if err != nil {
		t.Fatalf("list multipart post-abort: %v", err)
	}
	if len(mpList.Uploads) != 0 {
		t.Fatalf("expected 0 backend multipart sessions after abort, got %d", len(mpList.Uploads))
	}
}

func mustReq(t *testing.T, method, url string, body io.Reader) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	return req
}

func newRawAdminClient(endpoint, ak, sk string) *awss3.Client {
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

func ptrString(s string) *string { return &s }
