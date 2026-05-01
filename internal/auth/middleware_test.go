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

// stubStore is a tiny CredentialsStore for middleware tests — it returns
// a single fixed credential for the configured access key and
// ErrNoSuchCredential for everything else.
type stubStore struct{ cred Credential }

func (s stubStore) Lookup(_ context.Context, ak string) (*Credential, error) {
	if s.cred.AccessKey != ak {
		return nil, ErrNoSuchCredential
	}
	return &s.cred, nil
}

// signRequest computes a valid SigV4 Authorization header for req using
// the supplied secret + signed-header set. Mutates req.Header in place.
// Test helper — does not match real client signing behaviour exactly
// (e.g. it does not normalise host case), but is sufficient for the
// middleware-level checks below.
func signRequest(t *testing.T, req *http.Request, secret string, signedHeaders []string) {
	t.Helper()
	date := req.Header.Get("X-Amz-Date")
	if date == "" {
		t.Fatalf("signRequest: X-Amz-Date header required")
	}
	day := date[:8]
	bodyHash := req.Header.Get("X-Amz-Content-Sha256")
	if bodyHash == "" {
		bodyHash = unsignedBody
	}
	parsed := &parsedAuth{
		AccessKey:     "AKIDEXAMPLE",
		Date:          day,
		Region:        "us-east-1",
		Service:       "s3",
		SignedHeaders: signedHeaders,
	}
	canonical := canonicalRequest(req, signedHeaders, bodyHash)
	sig := computeSignature(secret, parsed, date, canonical)
	req.Header.Set("Authorization", fmt.Sprintf(
		"%s Credential=%s/%s/%s/%s/%s, SignedHeaders=%s, Signature=%s",
		sigAlgorithm, parsed.AccessKey, day, parsed.Region, parsed.Service,
		sigTerminator, strings.Join(signedHeaders, ";"), sig,
	))
}

// TestMiddlewareTrailerFormat_Rejected verifies that a streaming PUT
// carrying an x-amz-trailer header is rejected with
// ErrTrailerFormatUnsupported (US-003). The s3api layer translates that
// to 501 NotImplemented; this test asserts the auth-layer error so the
// translation chain stays small and verifiable.
func TestMiddlewareTrailerFormat_Rejected(t *testing.T) {
	const secret = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
	now := time.Now().UTC().Format(sigTimeFormat)

	req := httptest.NewRequest(http.MethodPut, "https://bucket.example.com/key", strings.NewReader(""))
	req.Host = "bucket.example.com"
	req.Header.Set("X-Amz-Date", now)
	req.Header.Set("X-Amz-Content-Sha256", streamingBody)
	req.Header.Set("X-Amz-Trailer", "x-amz-checksum-sha256")
	signRequest(t, req, secret, []string{"host", "x-amz-content-sha256", "x-amz-date", "x-amz-trailer"})

	m := &Middleware{
		Store: stubStore{cred: Credential{AccessKey: "AKIDEXAMPLE", Secret: secret, Owner: "test"}},
		Mode:  ModeRequired,
	}
	_, err := m.validateHeader(req)
	if !errors.Is(err, ErrTrailerFormatUnsupported) {
		t.Fatalf("validateHeader err = %v, want ErrTrailerFormatUnsupported", err)
	}
}

// TestMiddlewareTrailerFormat_UnsignedTrailerSentinel covers the
// STREAMING-UNSIGNED-PAYLOAD-TRAILER variant — same trailer header
// presence triggers the 501 path.
func TestMiddlewareTrailerFormat_UnsignedTrailerSentinel(t *testing.T) {
	const secret = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
	now := time.Now().UTC().Format(sigTimeFormat)

	req := httptest.NewRequest(http.MethodPut, "https://bucket.example.com/key", strings.NewReader(""))
	req.Host = "bucket.example.com"
	req.Header.Set("X-Amz-Date", now)
	req.Header.Set("X-Amz-Content-Sha256", streamingUnsignedTrailer)
	req.Header.Set("X-Amz-Trailer", "x-amz-checksum-crc32")
	signRequest(t, req, secret, []string{"host", "x-amz-content-sha256", "x-amz-date", "x-amz-trailer"})

	m := &Middleware{
		Store: stubStore{cred: Credential{AccessKey: "AKIDEXAMPLE", Secret: secret, Owner: "test"}},
		Mode:  ModeRequired,
	}
	_, err := m.validateHeader(req)
	if !errors.Is(err, ErrTrailerFormatUnsupported) {
		t.Fatalf("validateHeader err = %v, want ErrTrailerFormatUnsupported", err)
	}
}

// TestMiddlewareTrailerFormat_NoTrailerHeader_PassesThrough verifies the
// "behavior unchanged" half of the AC: a streaming PUT without
// x-amz-trailer flows through to the streaming reader as before, with no
// trailer-format error.
func TestMiddlewareTrailerFormat_NoTrailerHeader_PassesThrough(t *testing.T) {
	const secret = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
	now := time.Now().UTC().Format(sigTimeFormat)

	req := httptest.NewRequest(http.MethodPut, "https://bucket.example.com/key", strings.NewReader(""))
	req.Host = "bucket.example.com"
	req.Header.Set("X-Amz-Date", now)
	req.Header.Set("X-Amz-Content-Sha256", streamingBody)
	signRequest(t, req, secret, []string{"host", "x-amz-content-sha256", "x-amz-date"})

	m := &Middleware{
		Store: stubStore{cred: Credential{AccessKey: "AKIDEXAMPLE", Secret: secret, Owner: "test"}},
		Mode:  ModeRequired,
	}
	info, err := m.validateHeader(req)
	if err != nil {
		t.Fatalf("validateHeader err = %v, want nil", err)
	}
	if info == nil || info.AccessKey != "AKIDEXAMPLE" {
		t.Fatalf("validateHeader info = %+v, want AccessKey=AKIDEXAMPLE", info)
	}
}
