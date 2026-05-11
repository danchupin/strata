package s3

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/danchupin/strata/internal/data"
)

// TestPutChunksRoutesPerClassAcrossClusters is the headline US-003
// regression: a multi-cluster Backend (two endpoints, three classes
// where class A and class B share cluster-eu under different buckets,
// class C lives on cluster-us) must direct each PutChunks call at the
// correct (cluster, bucket) pair. Each cluster runs its own in-process
// http.Handler so we can assert the SDK call's Host + path bucket
// prefix without depending on a real S3.
func TestPutChunksRoutesPerClassAcrossClusters(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "ak")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "sk")

	eu := newCapturingS3Server(t)
	us := newCapturingS3Server(t)
	t.Cleanup(eu.Close)
	t.Cleanup(us.Close)

	cfg := Config{
		Clusters: map[string]S3ClusterSpec{
			"eu": {
				Endpoint:       eu.URL(),
				Region:         "eu-west-1",
				ForcePathStyle: true,
				Credentials:    CredentialsRef{Type: CredentialsChain},
			},
			"us": {
				Endpoint:       us.URL(),
				Region:         "us-east-1",
				ForcePathStyle: true,
				Credentials:    CredentialsRef{Type: CredentialsChain},
			},
		},
		Classes: map[string]ClassSpec{
			"A": {Cluster: "eu", Bucket: "bucket-a"},
			"B": {Cluster: "eu", Bucket: "bucket-b"},
			"C": {Cluster: "us", Bucket: "bucket-c"},
		},
		SkipCredsCheck: true,
	}
	b, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()

	for _, tc := range []struct {
		class, wantBucket string
		wantServer        *capturingS3Server
		otherServer       *capturingS3Server
	}{
		{"A", "bucket-a", eu, us},
		{"B", "bucket-b", eu, us},
		{"C", "bucket-c", us, eu},
	} {
		t.Run("class_"+tc.class, func(t *testing.T) {
			eu.reset()
			us.reset()
			m, err := b.PutChunks(ctx, strings.NewReader("payload-"+tc.class), tc.class)
			if err != nil {
				t.Fatalf("PutChunks(%s): %v", tc.class, err)
			}
			if m.Class != tc.class {
				t.Fatalf("Manifest.Class: want %q, got %q", tc.class, m.Class)
			}
			if got := tc.wantServer.lastBucket(); got != tc.wantBucket {
				t.Fatalf("PutChunks(%s) hit bucket %q on routed server, want %q", tc.class, got, tc.wantBucket)
			}
			if tc.otherServer.requestCount() != 0 {
				t.Fatalf("PutChunks(%s) leaked to the unrelated cluster (%d calls)", tc.class, tc.otherServer.requestCount())
			}
		})
	}
}

// TestPutChunksUnknownClassReturnsErrUnknownStorageClass pins AC1: a
// class that isn't in the routing table must surface
// ErrUnknownStorageClass — never silently default onto whichever
// cluster happens to be configured.
func TestPutChunksUnknownClassReturnsErrUnknownStorageClass(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "ak")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "sk")
	srv := newCapturingS3Server(t)
	t.Cleanup(srv.Close)

	cfg := Config{
		Clusters: map[string]S3ClusterSpec{
			"primary": {Endpoint: srv.URL(), Region: "us-east-1", ForcePathStyle: true, Credentials: CredentialsRef{Type: CredentialsChain}},
		},
		Classes: map[string]ClassSpec{
			"STANDARD": {Cluster: "primary", Bucket: "hot"},
		},
		SkipCredsCheck: true,
	}
	b, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := b.PutChunks(context.Background(), strings.NewReader("x"), "GLACIER"); !errors.Is(err, ErrUnknownStorageClass) {
		t.Fatalf("PutChunks(GLACIER): want ErrUnknownStorageClass, got %v", err)
	}
}

// TestDeleteRoutesViaManifestClass pins the Delete(m) routing contract:
// the manifest's Class field selects (cluster, bucket); the SDK
// DeleteObject must hit the correct cluster's endpoint with the
// correct bucket in the URL path.
func TestDeleteRoutesViaManifestClass(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "ak")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "sk")

	eu := newCapturingS3Server(t)
	us := newCapturingS3Server(t)
	t.Cleanup(eu.Close)
	t.Cleanup(us.Close)

	cfg := Config{
		Clusters: map[string]S3ClusterSpec{
			"eu": {Endpoint: eu.URL(), Region: "eu-west-1", ForcePathStyle: true, Credentials: CredentialsRef{Type: CredentialsChain}},
			"us": {Endpoint: us.URL(), Region: "us-east-1", ForcePathStyle: true, Credentials: CredentialsRef{Type: CredentialsChain}},
		},
		Classes: map[string]ClassSpec{
			"HOT":  {Cluster: "eu", Bucket: "hot"},
			"COLD": {Cluster: "us", Bucket: "cold"},
		},
		SkipCredsCheck: true,
	}
	b, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()

	hotManifest := &data.Manifest{
		Class:      "HOT",
		BackendRef: &data.BackendRef{Backend: BackendName, Key: "bucket-uuid/obj-uuid"},
	}
	eu.reset()
	us.reset()
	if err := b.Delete(ctx, hotManifest); err != nil {
		t.Fatalf("Delete HOT: %v", err)
	}
	if got := eu.lastBucket(); got != "hot" {
		t.Fatalf("Delete HOT hit bucket %q on eu, want hot", got)
	}
	if us.requestCount() != 0 {
		t.Fatalf("Delete HOT leaked to us (%d calls)", us.requestCount())
	}

	coldManifest := &data.Manifest{
		Class:      "COLD",
		BackendRef: &data.BackendRef{Backend: BackendName, Key: "bucket-uuid/obj-uuid"},
	}
	eu.reset()
	us.reset()
	if err := b.Delete(ctx, coldManifest); err != nil {
		t.Fatalf("Delete COLD: %v", err)
	}
	if got := us.lastBucket(); got != "cold" {
		t.Fatalf("Delete COLD hit bucket %q on us, want cold", got)
	}
	if eu.requestCount() != 0 {
		t.Fatalf("Delete COLD leaked to eu (%d calls)", eu.requestCount())
	}
}

// TestMultipartHandleCarriesClusterAndBucket pins AC2: the handle
// returned by CreateBackendMultipart encodes the routing target so
// UploadPart / Complete / Abort can re-resolve without the gateway
// passing class on every call. Two clusters + two classes — verify
// the handle's cluster + bucket match the requested class.
func TestMultipartHandleCarriesClusterAndBucket(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "ak")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "sk")

	eu := newCapturingS3Server(t)
	us := newCapturingS3Server(t)
	t.Cleanup(eu.Close)
	t.Cleanup(us.Close)
	// Both servers reply with the synthetic multipart create response —
	// reuse the syntheticMultipartServer here keyed off path-style bucket
	// in the request URL.
	eu.handler.multipart = true
	us.handler.multipart = true

	cfg := Config{
		Clusters: map[string]S3ClusterSpec{
			"eu": {Endpoint: eu.URL(), Region: "eu-west-1", ForcePathStyle: true, Credentials: CredentialsRef{Type: CredentialsChain}},
			"us": {Endpoint: us.URL(), Region: "us-east-1", ForcePathStyle: true, Credentials: CredentialsRef{Type: CredentialsChain}},
		},
		Classes: map[string]ClassSpec{
			"HOT":  {Cluster: "eu", Bucket: "hot"},
			"COLD": {Cluster: "us", Bucket: "cold"},
		},
		SkipCredsCheck: true,
	}
	b, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()

	hotHandle, err := b.CreateBackendMultipart(ctx, "HOT")
	if err != nil {
		t.Fatalf("CreateBackendMultipart HOT: %v", err)
	}
	cluster, bucket, key, uploadID, err := decodeHandle(hotHandle)
	if err != nil {
		t.Fatalf("decode HOT handle: %v", err)
	}
	if cluster != "eu" || bucket != "hot" {
		t.Fatalf("HOT handle routes to (%q,%q), want (eu, hot)", cluster, bucket)
	}
	if key == "" || uploadID == "" {
		t.Fatalf("HOT handle missing key/uploadID: %+v / %+v", key, uploadID)
	}

	coldHandle, err := b.CreateBackendMultipart(ctx, "COLD")
	if err != nil {
		t.Fatalf("CreateBackendMultipart COLD: %v", err)
	}
	cluster, bucket, _, _, err = decodeHandle(coldHandle)
	if err != nil {
		t.Fatalf("decode COLD handle: %v", err)
	}
	if cluster != "us" || bucket != "cold" {
		t.Fatalf("COLD handle routes to (%q,%q), want (us, cold)", cluster, bucket)
	}
}

// capturingS3Server is an httptest.Server that records the bucket from
// path-style requests and replies with synthetic-success responses for
// PutObject, DeleteObject, and (when multipart=true) the CreateMultipart
// XML body.
type capturingS3Server struct {
	*httptest.Server
	handler *capturingHandler
}

func newCapturingS3Server(t *testing.T) *capturingS3Server {
	t.Helper()
	h := &capturingHandler{}
	srv := httptest.NewServer(h)
	return &capturingS3Server{Server: srv, handler: h}
}

func (c *capturingS3Server) URL() string { return c.Server.URL }

func (c *capturingS3Server) lastBucket() string {
	c.handler.mu.Lock()
	defer c.handler.mu.Unlock()
	return c.handler.lastBucket
}

func (c *capturingS3Server) requestCount() int {
	c.handler.mu.Lock()
	defer c.handler.mu.Unlock()
	return c.handler.count
}

func (c *capturingS3Server) reset() {
	c.handler.mu.Lock()
	defer c.handler.mu.Unlock()
	c.handler.lastBucket = ""
	c.handler.count = 0
}

type capturingHandler struct {
	mu         sync.Mutex
	lastBucket string
	count      int
	multipart  bool
}

func (h *capturingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Body != nil {
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
	}
	bucket := bucketFromPath(r.URL.Path)
	h.mu.Lock()
	h.lastBucket = bucket
	h.count++
	multipart := h.multipart
	h.mu.Unlock()

	q := r.URL.Query()
	if multipart && r.Method == http.MethodPost && q.Has("uploads") {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<InitiateMultipartUploadResult><Bucket>` + bucket + `</Bucket><Key>` + r.URL.Path[1:] + `</Key><UploadId>synthetic</UploadId></InitiateMultipartUploadResult>`))
		return
	}
	// Default: PutObject / DeleteObject success.
	w.Header().Set("ETag", `"synthetic-etag"`)
	w.WriteHeader(http.StatusOK)
}

func bucketFromPath(p string) string {
	p = strings.TrimPrefix(p, "/")
	if i := strings.IndexByte(p, '/'); i > 0 {
		return p[:i]
	}
	return p
}
