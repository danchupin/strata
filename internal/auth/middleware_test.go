package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stubStore returns a fixed credential for any access key.
type stubStore struct {
	cred *Credential
	err  error
}

func (s *stubStore) Lookup(_ context.Context, _ string) (*Credential, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.cred, nil
}

func captureIdentity() (http.Handler, **AuthInfo) {
	var captured *AuthInfo
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = FromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	})
	return h, &captured
}

func denyToStatus(w http.ResponseWriter, _ *http.Request, _ error) {
	w.WriteHeader(http.StatusForbidden)
}

// US-024: optional mode + no auth header + no presigned query → AnonymousIdentity.
func TestMiddleware_OptionalMode_AnonymousPassthrough(t *testing.T) {
	mw := &Middleware{
		Store: &stubStore{err: ErrNoSuchCredential},
		Mode:  ModeOptional,
	}
	next, captured := captureIdentity()
	wrapped := mw.Wrap(next, denyToStatus)

	req := httptest.NewRequest("GET", "http://example/bkt/key", nil)
	rr := httptest.NewRecorder()
	wrapped.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status: got %d want %d", rr.Code, http.StatusNoContent)
	}
	info := *captured
	if info == nil || !info.IsAnonymous {
		t.Fatalf("expected anonymous identity, got %+v", info)
	}
	if info.Owner != "anonymous" {
		t.Fatalf("anonymous owner: got %q want anonymous", info.Owner)
	}
}

// US-024: optional mode + Authorization header present → middleware still
// validates the signature (and rejects when invalid) instead of bypassing.
func TestMiddleware_OptionalMode_HeaderStillValidated(t *testing.T) {
	mw := &Middleware{
		Store: &stubStore{cred: &Credential{AccessKey: "AKIA", Secret: "x", Owner: "alice"}},
		Mode:  ModeOptional,
	}
	next, _ := captureIdentity()
	wrapped := mw.Wrap(next, denyToStatus)

	req := httptest.NewRequest("GET", "http://example/bkt/key", nil)
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIA/20260101/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=deadbeef")
	rr := httptest.NewRecorder()
	wrapped.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 on bad signature in optional mode, got %d", rr.Code)
	}
}

// US-024: optional mode + presigned query → middleware also validates instead
// of treating the request as anonymous.
func TestMiddleware_OptionalMode_PresignedStillValidated(t *testing.T) {
	mw := &Middleware{
		Store: &stubStore{cred: &Credential{AccessKey: "AKIA", Secret: "x", Owner: "alice"}},
		Mode:  ModeOptional,
	}
	next, _ := captureIdentity()
	wrapped := mw.Wrap(next, denyToStatus)

	req := httptest.NewRequest("GET", "http://example/bkt/key?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Signature=deadbeef", nil)
	rr := httptest.NewRecorder()
	wrapped.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 on bad presigned in optional mode, got %d", rr.Code)
	}
}

// US-024: required mode rejects requests without an Authorization header.
func TestMiddleware_RequiredMode_NoAuthHeaderRejected(t *testing.T) {
	mw := &Middleware{
		Store: &stubStore{err: ErrNoSuchCredential},
		Mode:  ModeRequired,
	}
	next, _ := captureIdentity()
	wrapped := mw.Wrap(next, denyToStatus)

	req := httptest.NewRequest("GET", "http://example/bkt/key", nil)
	rr := httptest.NewRecorder()
	wrapped.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 in required mode without creds, got %d", rr.Code)
	}
}

// US-024: disabled mode (alias of off) treats every request as anonymous,
// even when an Authorization header is present.
func TestMiddleware_DisabledMode_AlwaysAnonymous(t *testing.T) {
	mw := &Middleware{
		Store: &stubStore{err: ErrNoSuchCredential},
		Mode:  ModeDisabled,
	}
	next, captured := captureIdentity()
	wrapped := mw.Wrap(next, denyToStatus)

	req := httptest.NewRequest("GET", "http://example/bkt/key", nil)
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIA/anything")
	rr := httptest.NewRecorder()
	wrapped.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status: got %d want %d", rr.Code, http.StatusNoContent)
	}
	info := *captured
	if info == nil || !info.IsAnonymous {
		t.Fatalf("expected anonymous identity, got %+v", info)
	}
}

// US-024: ParseMode covers each documented spelling, including the aliases.
func TestParseMode_Optional(t *testing.T) {
	cases := map[string]Mode{
		"":         ModeOff,
		"off":      ModeOff,
		"disabled": ModeDisabled,
		"required": ModeRequired,
		"optional": ModeOptional,
	}
	for in, want := range cases {
		got, err := ParseMode(in)
		if err != nil {
			t.Errorf("ParseMode(%q) err: %v", in, err)
		}
		if got != want {
			t.Errorf("ParseMode(%q) = %v want %v", in, got, want)
		}
	}
	if _, err := ParseMode("garbage"); err == nil || !strings.Contains(err.Error(), "unknown auth mode") {
		t.Fatalf("expected unknown-mode error, got %v", err)
	}
}

// AnonymousIdentity returns a fresh, non-shared instance so callers can mutate
// without poisoning the next request's identity.
func TestAnonymousIdentity_Fresh(t *testing.T) {
	a := AnonymousIdentity()
	a.Owner = "tampered"
	b := AnonymousIdentity()
	if b.Owner != "anonymous" || !b.IsAnonymous {
		t.Fatalf("anonymous identity reused across calls: %+v", b)
	}
}
