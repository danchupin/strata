package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// signRequest mints an Authorization header signing the canonical
// request with the given secret. Tests reuse this for both the IAM-
// access-key path (secret = cred.Secret) and the per-bucket signing
// path (secret = hex(DEK)).
func signRequest(t *testing.T, req *http.Request, accessKey, secret, date, region, service string) {
	t.Helper()
	parsed := &parsedAuth{
		AccessKey:     accessKey,
		Date:          date[:8],
		Region:        region,
		Service:       service,
		SignedHeaders: []string{"host", "x-amz-date"},
	}
	req.Header.Set("X-Amz-Date", date)
	canonical := canonicalRequest(req, parsed.SignedHeaders, emptyBodyHash)
	sig := computeSignature(secret, parsed, date, canonical)
	req.Header.Set("X-Amz-Content-Sha256", emptyBodyHash)
	req.Header.Set("Authorization", fmt.Sprintf(
		"%s Credential=%s/%s/%s/%s/%s, SignedHeaders=%s, Signature=%s",
		sigAlgorithm, parsed.AccessKey, parsed.Date, parsed.Region, parsed.Service, sigTerminator,
		strings.Join(parsed.SignedHeaders, ";"),
		sig,
	))
}

func freshDate() string { return time.Now().UTC().Format(sigTimeFormat) }

func TestMiddlewarePerBucketSigningKeyAcceptsDEK(t *testing.T) {
	dek := []byte{0xDE, 0xAD, 0xBE, 0xEF, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0A, 0x0B, 0x0C}
	store := &fakeKeyStore{wrapped: []byte{0xAB}, keyID: "kms-1"}
	kms := &fakeKMS{plaintext: dek}
	mw := &Middleware{
		Store: &stubStore{cred: &Credential{AccessKey: "AKIA", Secret: "ignored", Owner: "alice"}},
		Mode:  ModeRequired,
		BucketSigning: &BucketSigningResolver{
			Store:    store,
			KMS:      kms,
			Provider: "aws_kms",
			Cache:    NewDEKCache(5 * time.Minute),
		},
	}
	next, captured := captureIdentity()
	wrapped := mw.Wrap(next, denyToStatus)

	date := freshDate()
	req := httptest.NewRequest("GET", "http://example/bkt/key", nil)
	req.Host = "example"
	// Client signs with the DEK hex (the operator already has it from the
	// admin Rotate response).
	signRequest(t, req, "AKIA", encodeDEKAsSecret(dek), date, "us-east-1", "s3")
	rr := httptest.NewRecorder()
	wrapped.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", rr.Code)
	}
	info := *captured
	if info == nil || info.Owner != "alice" {
		t.Fatalf("identity: %+v", info)
	}
}

func TestMiddlewarePerBucketSigningKeyRejectsIAMSecret(t *testing.T) {
	dek := []byte{0xDE, 0xAD, 0xBE, 0xEF}
	store := &fakeKeyStore{wrapped: []byte{0xAB}, keyID: "kms-1"}
	kms := &fakeKMS{plaintext: dek}
	mw := &Middleware{
		Store: &stubStore{cred: &Credential{AccessKey: "AKIA", Secret: "iam-secret", Owner: "alice"}},
		Mode:  ModeRequired,
		BucketSigning: &BucketSigningResolver{
			Store: store, KMS: kms, Provider: "aws_kms", Cache: NewDEKCache(5 * time.Minute),
		},
	}
	next, _ := captureIdentity()
	wrapped := mw.Wrap(next, denyToStatus)
	date := freshDate()

	req := httptest.NewRequest("GET", "http://example/bkt/key", nil)
	req.Host = "example"
	// Client mistakenly signs with the IAM access-key secret — should
	// be rejected because the per-bucket DEK replaces it server-side.
	signRequest(t, req, "AKIA", "iam-secret", date, "us-east-1", "s3")
	rr := httptest.NewRecorder()
	wrapped.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 on mismatched secret, got %d", rr.Code)
	}
}

func TestMiddlewareSigningKeyNotSetFallsThrough(t *testing.T) {
	store := &fakeKeyStore{err: ErrBucketSigningKeyNotSet}
	mw := &Middleware{
		Store: &stubStore{cred: &Credential{AccessKey: "AKIA", Secret: "iam-secret", Owner: "alice"}},
		Mode:  ModeRequired,
		BucketSigning: &BucketSigningResolver{
			Store: store, Provider: "aws_kms",
		},
	}
	next, captured := captureIdentity()
	wrapped := mw.Wrap(next, denyToStatus)
	date := freshDate()

	req := httptest.NewRequest("GET", "http://example/bkt/key", nil)
	req.Host = "example"
	// No per-bucket key — client signs with IAM secret (today's path).
	signRequest(t, req, "AKIA", "iam-secret", date, "us-east-1", "s3")
	rr := httptest.NewRecorder()
	wrapped.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", rr.Code)
	}
	if (*captured).Owner != "alice" {
		t.Fatalf("identity: %+v", *captured)
	}
}

func TestMiddlewareKMSUnavailableSurfaces(t *testing.T) {
	store := &fakeKeyStore{wrapped: []byte{0xAB}, keyID: "kms-1"}
	// KMS is nil → ErrKMSUnavailable.
	mw := &Middleware{
		Store: &stubStore{cred: &Credential{AccessKey: "AKIA", Secret: "iam-secret", Owner: "alice"}},
		Mode:  ModeRequired,
		BucketSigning: &BucketSigningResolver{
			Store: store, KMS: nil, Provider: "aws_kms",
		},
	}
	var seenErr error
	deny := func(w http.ResponseWriter, _ *http.Request, err error) {
		seenErr = err
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	next, _ := captureIdentity()
	wrapped := mw.Wrap(next, deny)
	date := freshDate()

	req := httptest.NewRequest("GET", "http://example/bkt/key", nil)
	req.Host = "example"
	signRequest(t, req, "AKIA", "iam-secret", date, "us-east-1", "s3")
	rr := httptest.NewRecorder()
	wrapped.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rr.Code)
	}
	if !errors.Is(seenErr, ErrKMSUnavailable) {
		t.Fatalf("expected ErrKMSUnavailable, got %v", seenErr)
	}
}

// withFreshTimestampForBucketTest is a no-op helper kept to avoid name
// clash with the streaming_trailer_test.go helper; the bucket-signing
// tests use freshDate inline.
var _ = context.Background
