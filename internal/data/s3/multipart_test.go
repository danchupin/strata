package s3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danchupin/strata/internal/data"
)

// TestStubMultipartReturnsErrUnsupported guards US-010 against a New()
// stub (no Open): every MultipartBackend method must surface
// errors.ErrUnsupported instead of silently no-op'ing or panicking.
func TestStubMultipartReturnsErrUnsupported(t *testing.T) {
	b := New()
	ctx := context.Background()

	if _, err := b.CreateBackendMultipart(ctx, "STANDARD"); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("CreateBackendMultipart: want ErrUnsupported, got %v", err)
	}
	if _, err := b.UploadBackendPart(ctx, "k\x00u", 1, strings.NewReader(""), 0); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("UploadBackendPart: want ErrUnsupported, got %v", err)
	}
	if _, err := b.CompleteBackendMultipart(ctx, "k\x00u", []data.BackendCompletedPart{{PartNumber: 1, ETag: "e"}}, "STANDARD"); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("CompleteBackendMultipart: want ErrUnsupported, got %v", err)
	}
	if err := b.AbortBackendMultipart(ctx, "k\x00u"); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("AbortBackendMultipart: want ErrUnsupported, got %v", err)
	}
}

// TestHandleEncodeDecodeRoundTrip pins the opaque-handle invariant:
// encode/decode is a stable round-trip and split on the NUL separator
// returns the original two components.
func TestHandleEncodeDecodeRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		key, uploadID string
	}{
		{"buck/obj", "abc"},
		{"prefix/with-slash/obj-uuid", "QWERTY=="},
	} {
		h := encodeHandle(tc.key, tc.uploadID)
		k, u, err := decodeHandle(h)
		if err != nil {
			t.Fatalf("decode %q: %v", h, err)
		}
		if k != tc.key || u != tc.uploadID {
			t.Fatalf("round-trip mismatch: got (%q,%q), want (%q,%q)", k, u, tc.key, tc.uploadID)
		}
	}
}

// TestDecodeHandleRejectsMalformed pins the defensive guard: hand-crafted
// values without the NUL separator must fail rather than treating the
// whole string as the key with an empty upload-id.
func TestDecodeHandleRejectsMalformed(t *testing.T) {
	for _, h := range []string{"", "no-separator", "\x00uploadid", "key\x00"} {
		if _, _, err := decodeHandle(h); err == nil {
			t.Fatalf("decodeHandle(%q): want error, got nil", h)
		}
	}
}

// TestMultipartLifecycleAgainstSyntheticBackend exercises the full US-010
// pass-through against an in-process http.Handler that mimics the S3
// multipart protocol — no MinIO required. Asserts:
//  1. CreateMultipartUpload → SDK upload-id surfaces in handle.
//  2. UploadPart × 2 → per-part ETag returned to caller verbatim.
//  3. CompleteMultipartUpload → BackendRef-shape Manifest with the same
//     key as initiated, the backend's composite ETag, and an opaque
//     VersionID forwarded from the SDK.
//  4. The SDK calls hit exactly one Create + N UploadPart + one Complete.
func TestMultipartLifecycleAgainstSyntheticBackend(t *testing.T) {
	server := newSyntheticMultipartServer()
	transport := &httpHandlerTransport{handler: server}

	cfg := Config{
		Bucket:         "strata-test",
		Region:         "us-east-1",
		Endpoint:       "http://example.invalid",
		AccessKey:      "ak",
		SecretKey:      "sk",
		ForcePathStyle: true,
		SkipProbe:      true,
		HTTPClient:     &http.Client{Transport: transport},
	}
	ctx := context.Background()
	b, err := Open(ctx, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	handle, err := b.CreateBackendMultipart(ctx, "STANDARD")
	if err != nil {
		t.Fatalf("CreateBackendMultipart: %v", err)
	}
	key, uploadID, err := decodeHandle(handle)
	if err != nil {
		t.Fatalf("decode handle: %v", err)
	}
	if uploadID != server.uploadID {
		t.Fatalf("handle upload-id %q, want %q", uploadID, server.uploadID)
	}
	if !strings.Contains(key, "/") {
		t.Fatalf("backend object key must include bucket-uuid/object-uuid prefix, got %q", key)
	}

	etag1, err := b.UploadBackendPart(ctx, handle, 1, strings.NewReader("part-1"), 6)
	if err != nil {
		t.Fatalf("UploadBackendPart 1: %v", err)
	}
	if etag1 != server.partETag(1) {
		t.Fatalf("part 1 etag: got %q, want %q", etag1, server.partETag(1))
	}
	etag2, err := b.UploadBackendPart(ctx, handle, 2, strings.NewReader("part-2"), 6)
	if err != nil {
		t.Fatalf("UploadBackendPart 2: %v", err)
	}
	if etag2 != server.partETag(2) {
		t.Fatalf("part 2 etag: got %q, want %q", etag2, server.partETag(2))
	}

	m, err := b.CompleteBackendMultipart(ctx, handle, []data.BackendCompletedPart{
		{PartNumber: 1, ETag: etag1},
		{PartNumber: 2, ETag: etag2},
	}, "STANDARD")
	if err != nil {
		t.Fatalf("CompleteBackendMultipart: %v", err)
	}
	if m.BackendRef == nil {
		t.Fatal("Manifest.BackendRef nil after CompleteBackendMultipart")
	}
	if m.BackendRef.Key != key {
		t.Fatalf("BackendRef.Key %q, want %q", m.BackendRef.Key, key)
	}
	if m.BackendRef.ETag != server.completeETag {
		t.Fatalf("BackendRef.ETag %q, want %q", m.BackendRef.ETag, server.completeETag)
	}
	if m.BackendRef.VersionID != server.completeVersionID {
		t.Fatalf("BackendRef.VersionID %q, want %q", m.BackendRef.VersionID, server.completeVersionID)
	}
	if len(m.Chunks) != 0 {
		t.Fatalf("Manifest.Chunks must be empty for backend pass-through (1:1 invariant), got %d", len(m.Chunks))
	}

	if got := server.requestCount("CreateMultipartUpload"); got != 1 {
		t.Fatalf("CreateMultipartUpload count: got %d, want 1", got)
	}
	if got := server.requestCount("UploadPart"); got != 2 {
		t.Fatalf("UploadPart count: got %d, want 2", got)
	}
	if got := server.requestCount("CompleteMultipartUpload"); got != 1 {
		t.Fatalf("CompleteMultipartUpload count: got %d, want 1", got)
	}

	// AbortMultipartUpload after Complete is the NoSuchUpload code-path —
	// idempotent abort must absorb it and return nil.
	if err := b.AbortBackendMultipart(ctx, handle); err != nil {
		t.Fatalf("AbortBackendMultipart on completed handle (NoSuchUpload idempotent): %v", err)
	}
}

// httpHandlerTransport bridges aws-sdk-go-v2's http.Client to an http.Handler
// (e.g. one served by httptest.NewRecorder) so SDK requests can be answered
// by hand-rolled S3-protocol XML without spinning up a real listener.
type httpHandlerTransport struct {
	handler http.Handler
}

func (t *httpHandlerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	t.handler.ServeHTTP(rec, req)
	return rec.Result(), nil
}

type syntheticMultipartServer struct {
	uploadID          string
	completeETag      string
	completeVersionID string
	counts            map[string]int
}

func newSyntheticMultipartServer() *syntheticMultipartServer {
	return &syntheticMultipartServer{
		uploadID:          "synthetic-upload-id-7",
		completeETag:      "deadbeef-2",
		completeVersionID: "v-uuid",
		counts:            map[string]int{},
	}
}

func (s *syntheticMultipartServer) partETag(part int) string {
	return "etag-" + strings.Repeat("a", part)
}

func (s *syntheticMultipartServer) requestCount(op string) int { return s.counts[op] }

func (s *syntheticMultipartServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if r.Body != nil {
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
	}

	switch {
	case r.Method == http.MethodPost && q.Has("uploads"):
		s.counts["CreateMultipartUpload"]++
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<InitiateMultipartUploadResult><Bucket>strata-test</Bucket><Key>` + r.URL.Path[1:] + `</Key><UploadId>` + s.uploadID + `</UploadId></InitiateMultipartUploadResult>`))
		return

	case r.Method == http.MethodPut && q.Get("partNumber") != "" && q.Get("uploadId") != "":
		s.counts["UploadPart"]++
		var partN int
		_, _ = fmt.Sscanf(q.Get("partNumber"), "%d", &partN)
		w.Header().Set("ETag", `"`+s.partETag(partN)+`"`)
		w.WriteHeader(http.StatusOK)
		return

	case r.Method == http.MethodPost && q.Get("uploadId") != "":
		s.counts["CompleteMultipartUpload"]++
		w.Header().Set("Content-Type", "application/xml")
		if s.completeVersionID != "" {
			w.Header().Set("x-amz-version-id", s.completeVersionID)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<CompleteMultipartUploadResult><Bucket>strata-test</Bucket><Key>` + r.URL.Path[1:] + `</Key><ETag>"` + s.completeETag + `"</ETag></CompleteMultipartUploadResult>`))
		return

	case r.Method == http.MethodDelete && q.Get("uploadId") != "":
		s.counts["AbortMultipartUpload"]++
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><Error><Code>NoSuchUpload</Code><Message>upload not found</Message><RequestId>r</RequestId><HostId>h</HostId></Error>`))
		return

	default:
		w.WriteHeader(http.StatusNotImplemented)
	}
}
