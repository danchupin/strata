package adminapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/crypto/kms"
)

// fakeKMSProvider is the test double for kms.Provider used by the
// signing-key admin tests. fixedDEK lets tests assert a specific
// secret_access_key roundtrip; failGen / failUnwrap inject error paths.
type fakeKMSProvider struct {
	dek           []byte
	wrapped       []byte
	genErr        error
	unwrapErr     error
	generateCalls int32
}

func (f *fakeKMSProvider) GenerateDataKey(ctx context.Context, keyID string) ([]byte, []byte, error) {
	atomic.AddInt32(&f.generateCalls, 1)
	if f.genErr != nil {
		return nil, nil, f.genErr
	}
	plain := make([]byte, len(f.dek))
	copy(plain, f.dek)
	wrapped := make([]byte, len(f.wrapped))
	copy(wrapped, f.wrapped)
	return plain, wrapped, nil
}

func (f *fakeKMSProvider) UnwrapDEK(ctx context.Context, keyID string, wrapped []byte) ([]byte, error) {
	if f.unwrapErr != nil {
		return nil, f.unwrapErr
	}
	out := make([]byte, len(f.dek))
	copy(out, f.dek)
	return out, nil
}

func newSigningKeyTestServer(t *testing.T, kmsP kms.Provider, cache *auth.DEKCache, maxAge time.Duration) *Server {
	t.Helper()
	s := newTestServer()
	s.SigningKey = SigningKeyConfig{
		Provider:     kmsP,
		Cache:        cache,
		DefaultKeyID: "test-cmk",
		MaxAge:       maxAge,
	}
	return s
}

func signingKeyRequest(t *testing.T, s *Server, method, path string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	req = req.WithContext(auth.WithAuth(req.Context(), &auth.AuthInfo{AccessKey: "ops", Owner: "ops"}))
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, req)
	return rr
}

func TestSigningKeyRotate_HappyPath(t *testing.T) {
	dek := []byte("0123456789abcdef0123456789abcdef")
	wrapped := []byte("wrapped-blob")
	kmsP := &fakeKMSProvider{dek: dek, wrapped: wrapped}
	cache := auth.NewDEKCache(5 * time.Minute)
	cache.Put("bkt", "stale", []byte{0xAA}) // prime cache so we can prove Invalidate ran.
	s := newSigningKeyTestServer(t, kmsP, cache, 24*time.Hour)
	if _, err := s.Meta.CreateBucket(context.Background(), "bkt", "alice", "STANDARD"); err != nil {
		t.Fatalf("create bucket: %v", err)
	}

	rr := signingKeyRequest(t, s, http.MethodPost, "/admin/v1/buckets/bkt/signing-key/rotate", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got signingKeyRotateResponse
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.KeyID != "test-cmk" {
		t.Fatalf("key_id: got %q want test-cmk", got.KeyID)
	}
	if len(got.SecretAccessKey) != len(dek)*2 {
		t.Fatalf("secret hex length: got %d want %d", len(got.SecretAccessKey), len(dek)*2)
	}
	if got.WrappedDEKLength != len(wrapped) {
		t.Fatalf("wrapped_dek_length: got %d want %d", got.WrappedDEKLength, len(wrapped))
	}
	if _, hit := cache.Get("bkt", "stale"); hit {
		t.Fatalf("cache: stale entry should have been invalidated")
	}

	// Meta persisted the wrapped form + key_id.
	persistedWrapped, persistedKeyID, _, err := s.Meta.GetBucketSigningKey(context.Background(), "bkt")
	if err != nil {
		t.Fatalf("meta read: %v", err)
	}
	if persistedKeyID != "test-cmk" {
		t.Fatalf("persisted key_id: got %q want test-cmk", persistedKeyID)
	}
	if !bytes.Equal(persistedWrapped, wrapped) {
		t.Fatalf("persisted wrapped mismatch")
	}
}

func TestSigningKeyRotate_OverrideKeyID(t *testing.T) {
	s := newSigningKeyTestServer(t, &fakeKMSProvider{dek: []byte("d"), wrapped: []byte("w")}, nil, 0)
	if _, err := s.Meta.CreateBucket(context.Background(), "bkt", "alice", "STANDARD"); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	body, _ := json.Marshal(signingKeyRotateRequest{KeyID: "alias/custom"})
	rr := signingKeyRequest(t, s, http.MethodPost, "/admin/v1/buckets/bkt/signing-key/rotate", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got signingKeyRotateResponse
	_ = json.NewDecoder(rr.Body).Decode(&got)
	if got.KeyID != "alias/custom" {
		t.Fatalf("override key_id: got %q want alias/custom", got.KeyID)
	}
}

func TestSigningKeyRotate_BucketMissing(t *testing.T) {
	s := newSigningKeyTestServer(t, &fakeKMSProvider{dek: []byte("d"), wrapped: []byte("w")}, nil, 0)
	rr := signingKeyRequest(t, s, http.MethodPost, "/admin/v1/buckets/missing/signing-key/rotate", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestSigningKeyRotate_KMSUnavailable(t *testing.T) {
	s := newSigningKeyTestServer(t, &fakeKMSProvider{genErr: kms.ErrKMSUnavailable}, nil, 0)
	if _, err := s.Meta.CreateBucket(context.Background(), "bkt", "alice", "STANDARD"); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	rr := signingKeyRequest(t, s, http.MethodPost, "/admin/v1/buckets/bkt/signing-key/rotate", nil)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Retry-After"); got != "30" {
		t.Fatalf("retry-after: got %q want 30", got)
	}
	var body errorResponse
	_ = json.NewDecoder(rr.Body).Decode(&body)
	if body.Code != "KMSUnavailable" {
		t.Fatalf("code: got %q want KMSUnavailable", body.Code)
	}
}

func TestSigningKeyRotate_NoProvider(t *testing.T) {
	s := newTestServer() // no SigningKey.Provider.
	if _, err := s.Meta.CreateBucket(context.Background(), "bkt", "alice", "STANDARD"); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	rr := signingKeyRequest(t, s, http.MethodPost, "/admin/v1/buckets/bkt/signing-key/rotate", nil)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var body errorResponse
	_ = json.NewDecoder(rr.Body).Decode(&body)
	if body.Code != "SigningKeyDisabled" {
		t.Fatalf("code: got %q want SigningKeyDisabled", body.Code)
	}
}

func TestSigningKeyStatus_NotSet(t *testing.T) {
	s := newSigningKeyTestServer(t, &fakeKMSProvider{}, nil, 24*time.Hour)
	if _, err := s.Meta.CreateBucket(context.Background(), "bkt", "alice", "STANDARD"); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	rr := signingKeyRequest(t, s, http.MethodGet, "/admin/v1/buckets/bkt/signing-key/status", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var body errorResponse
	_ = json.NewDecoder(rr.Body).Decode(&body)
	if body.Code != "NoSigningKey" {
		t.Fatalf("code: got %q want NoSigningKey", body.Code)
	}
}

func TestSigningKeyStatus_FreshKey(t *testing.T) {
	dek := []byte("0123456789abcdef0123456789abcdef")
	wrapped := []byte("wrapped")
	s := newSigningKeyTestServer(t, &fakeKMSProvider{dek: dek, wrapped: wrapped}, nil, 30*24*time.Hour)
	if _, err := s.Meta.CreateBucket(context.Background(), "bkt", "alice", "STANDARD"); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	if err := s.Meta.SetBucketSigningKey(context.Background(), "bkt", wrapped, "test-cmk"); err != nil {
		t.Fatalf("seed key: %v", err)
	}

	rr := signingKeyRequest(t, s, http.MethodGet, "/admin/v1/buckets/bkt/signing-key/status", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got signingKeyStatusResponse
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.KeyID != "test-cmk" {
		t.Fatalf("key_id: got %q", got.KeyID)
	}
	if got.Expired {
		t.Fatalf("expected expired=false on fresh key")
	}
	if got.MaxAgeDays < 29.9 || got.MaxAgeDays > 30.1 {
		t.Fatalf("max_age_days: got %f want ~30", got.MaxAgeDays)
	}
}

func TestSigningKeyStatus_ExpiredFlag(t *testing.T) {
	// Use memory store directly and pre-set createdAt by re-setting twice
	// with a short max-age window. Memory store stamps createdAt = now()
	// on Set, so we set, then advance the resolver's wall clock via the
	// MaxAge knob (set MaxAge tiny so even a "just set" row is expired).
	s := newSigningKeyTestServer(t, &fakeKMSProvider{}, nil, 1*time.Nanosecond)
	if _, err := s.Meta.CreateBucket(context.Background(), "bkt", "alice", "STANDARD"); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	if err := s.Meta.SetBucketSigningKey(context.Background(), "bkt", []byte("w"), "k"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	time.Sleep(time.Millisecond) // ensure age > 1ns.

	rr := signingKeyRequest(t, s, http.MethodGet, "/admin/v1/buckets/bkt/signing-key/status", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got signingKeyStatusResponse
	_ = json.NewDecoder(rr.Body).Decode(&got)
	if !got.Expired {
		t.Fatalf("expected expired=true on aged key, got %+v", got)
	}
}

func TestSigningKeyDelete_Idempotent(t *testing.T) {
	cache := auth.NewDEKCache(5 * time.Minute)
	cache.Put("bkt", "stale", []byte{0xAA})
	s := newSigningKeyTestServer(t, &fakeKMSProvider{}, cache, 0)
	if _, err := s.Meta.CreateBucket(context.Background(), "bkt", "alice", "STANDARD"); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	if err := s.Meta.SetBucketSigningKey(context.Background(), "bkt", []byte("w"), "k"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rr := signingKeyRequest(t, s, http.MethodDelete, "/admin/v1/buckets/bkt/signing-key", nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if _, hit := cache.Get("bkt", "stale"); hit {
		t.Fatalf("cache: stale entry should have been invalidated")
	}
	// Re-delete is idempotent.
	rr = signingKeyRequest(t, s, http.MethodDelete, "/admin/v1/buckets/bkt/signing-key", nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("re-delete status=%d body=%s", rr.Code, rr.Body.String())
	}
}

// Sanity: the rotate handler does NOT swallow a meta failure that
// arrives after the KMS round-trip succeeded — we want loud failure so
// the operator notices an inconsistent state instead of a quiet partial
// rotate.
func TestSigningKeyRotate_MetaErrorSurfaces(t *testing.T) {
	dek := []byte("d")
	wrapped := []byte("w")
	s := newSigningKeyTestServer(t, &fakeKMSProvider{dek: dek, wrapped: wrapped}, nil, 0)
	// No CreateBucket: SetBucketSigningKey will return ErrBucketNotFound.
	rr := signingKeyRequest(t, s, http.MethodPost, "/admin/v1/buckets/ghost/signing-key/rotate", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

// The auth-side max-age enforcement is independently covered by
// internal/auth tests; here we just verify the admin classifier maps
// the right kms.ErrKMSUnavailable into the operator-facing response.
func TestWriteKMSError_UnknownErrorIsInternal(t *testing.T) {
	s := newSigningKeyTestServer(t, &fakeKMSProvider{}, nil, 0)
	w := httptest.NewRecorder()
	s.writeKMSError(w, "RotateBucketSigningKey", errors.New("opaque kms hiccup"))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d", w.Code)
	}
}
