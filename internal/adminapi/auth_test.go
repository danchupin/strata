package adminapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/danchupin/strata/internal/auth"
)

// newAuthServer returns a Server with a single seeded credential
// (test-key / test-secret / owner=test) so login flows have something to
// authenticate against.
func newAuthServer() *Server {
	creds := auth.NewStaticStore(map[string]*auth.Credential{
		"test-key": {AccessKey: "test-key", Secret: "test-secret", Owner: "test"},
	})
	s := New(nil, creds, "test-sha", []byte("0123456789abcdef0123456789abcdef"))
	s.Started = time.Unix(1_700_000_000, 0)
	return s
}

func postJSON(h http.Handler, path string, body any, cookies ...*http.Cookie) *http.Response {
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr.Result()
}

func TestLoginSuccess(t *testing.T) {
	s := newAuthServer()
	resp := postJSON(s.Handler(), "/admin/v1/auth/login",
		loginRequest{AccessKey: "test-key", SecretKey: "test-secret"})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	var body sessionResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.AccessKey != "test-key" {
		t.Errorf("access_key: %q", body.AccessKey)
	}
	if body.ExpiresAt <= time.Now().Unix() {
		t.Errorf("expires_at: %d not in future", body.ExpiresAt)
	}
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookieName {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("session cookie not set")
	}
	if !cookie.HttpOnly {
		t.Error("cookie HttpOnly: want true")
	}
	if cookie.SameSite != http.SameSiteStrictMode {
		t.Errorf("cookie SameSite: got %v", cookie.SameSite)
	}
	if cookie.Path != "/admin" {
		t.Errorf("cookie Path: %q", cookie.Path)
	}
	if cookie.MaxAge != 86400 {
		t.Errorf("cookie MaxAge: %d", cookie.MaxAge)
	}
}

func TestLoginInvalidSecret(t *testing.T) {
	s := newAuthServer()
	resp := postJSON(s.Handler(), "/admin/v1/auth/login",
		loginRequest{AccessKey: "test-key", SecretKey: "wrong"})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status: %d want 401", resp.StatusCode)
	}
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookieName {
			t.Error("session cookie set on failed login")
		}
	}
}

func TestLoginUnknownAccessKey(t *testing.T) {
	s := newAuthServer()
	resp := postJSON(s.Handler(), "/admin/v1/auth/login",
		loginRequest{AccessKey: "ghost", SecretKey: "x"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status: %d", resp.StatusCode)
	}
}

func TestLoginMalformedBody(t *testing.T) {
	s := newAuthServer()
	req := httptest.NewRequest(http.MethodPost, "/admin/v1/auth/login",
		strings.NewReader("not-json"))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status: %d want 400", rr.Code)
	}
}

func TestLogoutClearsCookie(t *testing.T) {
	s := newAuthServer()
	req := httptest.NewRequest(http.MethodPost, "/admin/v1/auth/logout", nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: %d", rr.Code)
	}
	resp := rr.Result()
	defer resp.Body.Close()
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookieName {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("logout did not set clearing cookie")
	}
	if cookie.MaxAge >= 0 {
		t.Errorf("cookie MaxAge: %d want <0", cookie.MaxAge)
	}
}

func TestWhoamiUnauthenticated(t *testing.T) {
	s := newAuthServer()
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/auth/whoami", nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status: %d want 401", rr.Code)
	}
}

func TestWhoamiWithSessionCookie(t *testing.T) {
	s := newAuthServer()
	// Mint a session ourselves and present it.
	tok, _, err := signSession(s.JWTSecret, "test-key", time.Now())
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/auth/whoami", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: tok})
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}
	var body sessionResponse
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.AccessKey != "test-key" {
		t.Errorf("access_key: %q", body.AccessKey)
	}
	if body.ExpiresAt == 0 {
		t.Error("expires_at: want non-zero")
	}
}

func TestSessionCookieGrantsAccess(t *testing.T) {
	s := newAuthServer()
	// Login → cookie → call a stub /cluster/status with the cookie.
	resp := postJSON(s.Handler(), "/admin/v1/auth/login",
		loginRequest{AccessKey: "test-key", SecretKey: "test-secret"})
	resp.Body.Close()
	var sessionCookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookieName {
			sessionCookie = c
		}
	}
	if sessionCookie == nil {
		t.Fatal("login: no cookie")
	}
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/cluster/status", nil)
	req.AddCookie(sessionCookie)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("authenticated GET: %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestForgedSessionRejected(t *testing.T) {
	s := newAuthServer()
	// Sign with a different secret — must be rejected by the gateway.
	tok, _, err := signSession([]byte("wrong-secret-32-bytes-aaaaaaaaaa"), "test-key", time.Now())
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/cluster/status", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: tok})
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status: %d want 401", rr.Code)
	}
}

func TestExpiredSessionRejected(t *testing.T) {
	s := newAuthServer()
	tok, _, err := signSession(s.JWTSecret, "test-key", time.Now().Add(-25*time.Hour))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/cluster/status", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: tok})
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status: %d want 401", rr.Code)
	}
}

func TestVerifySessionRoundTrip(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	tok, claims, err := signSession(secret, "alice", time.Now())
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	got, err := verifySession(secret, tok)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got.Sub != "alice" {
		t.Errorf("sub: %q", got.Sub)
	}
	if got.Exp != claims.Exp {
		t.Errorf("exp: %d vs %d", got.Exp, claims.Exp)
	}
}
