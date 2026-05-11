package s3

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/danchupin/strata/internal/data"
)

// stubRoundTripper is a no-op http.RoundTripper. PresignGetObject doesn't
// hit the network — it computes the URL locally — so the transport never
// runs. Kept defensively in case future SDK versions reach for it.
type stubRoundTripper struct{}

func (stubRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(bytes.NewReader(nil)),
	}, nil
}

func newPresignTestBackend(t *testing.T) *Backend {
	t.Helper()
	endpoint := "http://minio.test:9000"
	t.Setenv("AWS_ACCESS_KEY_ID", "presign-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "presign-secret")
	b, err := Open(context.Background(), Config{
		Bucket:         "test-bucket",
		Region:         "us-east-1",
		Endpoint:       endpoint,
		AccessKey:      "presign-key",
		SecretKey:      "presign-secret",
		ForcePathStyle: true,
		HTTPClient:     &http.Client{Transport: stubRoundTripper{}},
		SkipProbe:      true,
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return b
}

func TestPresignGetObjectReturnsBackendURL(t *testing.T) {
	b := newPresignTestBackend(t)
	m := &data.Manifest{
		Class: "STANDARD",
		Size:  100,
		BackendRef: &data.BackendRef{
			Backend: BackendName,
			Key:     "bucket-uuid/object-uuid",
			Size:    100,
		},
	}
	urlStr, err := b.PresignGetObject(context.Background(), m, 5*time.Minute)
	if err != nil {
		t.Fatalf("presign: %v", err)
	}
	u, err := url.Parse(urlStr)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	if u.Host != "minio.test:9000" {
		t.Errorf("host: got %q want minio.test:9000", u.Host)
	}
	if !strings.Contains(u.Path, "test-bucket") || !strings.Contains(u.Path, "bucket-uuid/object-uuid") {
		t.Errorf("path missing bucket+key: %s", u.Path)
	}
	q := u.Query()
	if q.Get("X-Amz-Signature") == "" {
		t.Errorf("missing X-Amz-Signature: %s", urlStr)
	}
	if exp := q.Get("X-Amz-Expires"); exp != "300" {
		t.Errorf("X-Amz-Expires: got %q want 300", exp)
	}
}

func TestPresignGetObjectAppliesVersionId(t *testing.T) {
	b := newPresignTestBackend(t)
	m := &data.Manifest{
		BackendRef: &data.BackendRef{
			Backend:   BackendName,
			Key:       "bucket-uuid/object-uuid",
			VersionID: "11111111-1111-1111-1111-111111111111",
		},
	}
	urlStr, err := b.PresignGetObject(context.Background(), m, time.Minute)
	if err != nil {
		t.Fatalf("presign: %v", err)
	}
	u, _ := url.Parse(urlStr)
	if v := u.Query().Get("versionId"); v != "11111111-1111-1111-1111-111111111111" {
		t.Errorf("versionId in URL: got %q", v)
	}
}

func TestPresignGetObjectRequiresBackendRef(t *testing.T) {
	b := newPresignTestBackend(t)
	if _, err := b.PresignGetObject(context.Background(), nil, time.Minute); !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("nil manifest: got %v want ErrUnsupported", err)
	}
	chunks := &data.Manifest{Chunks: []data.ChunkRef{{Cluster: "c", Pool: "p", OID: "o"}}}
	if _, err := b.PresignGetObject(context.Background(), chunks, time.Minute); !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("chunks-shape: got %v want ErrUnsupported", err)
	}
}

func TestStubPresignReturnsErrUnsupported(t *testing.T) {
	b := &Backend{}
	m := &data.Manifest{BackendRef: &data.BackendRef{Backend: BackendName, Key: "k"}}
	if _, err := b.PresignGetObject(context.Background(), m, time.Minute); !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("stub presign: got %v want ErrUnsupported", err)
	}
}

func TestPresignGetObjectDefaultsExpires(t *testing.T) {
	b := newPresignTestBackend(t)
	m := &data.Manifest{BackendRef: &data.BackendRef{Backend: BackendName, Key: "k"}}
	urlStr, err := b.PresignGetObject(context.Background(), m, 0)
	if err != nil {
		t.Fatalf("presign: %v", err)
	}
	u, _ := url.Parse(urlStr)
	if exp := u.Query().Get("X-Amz-Expires"); exp != "900" {
		t.Errorf("default expires: got %q want 900 (15m)", exp)
	}
}
