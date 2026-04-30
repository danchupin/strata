//go:build integration

package s3api_test

import (
	"bytes"
	"context"
	mathrand "math/rand"
	"net/http"
	"net/http/httptest"
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
