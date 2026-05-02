package adminapi

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// sessionCookieName is the name of the JWT session cookie set by
// POST /admin/v1/auth/login. The console UI does not need to read it
// (it is HttpOnly) — the browser carries it on every /admin/v1/* request.
const sessionCookieName = "strata_session"

// sessionTTL controls cookie Max-Age and JWT exp. Per PRD US-004: 24 hours.
const sessionTTL = 24 * time.Hour

// sessionClaims is the JWT payload. HS256-signed by the gateway with
// STRATA_CONSOLE_JWT_SECRET.
type sessionClaims struct {
	Sub string `json:"sub"`
	Iat int64  `json:"iat"`
	Exp int64  `json:"exp"`
}

var (
	errSessionMalformed = errors.New("malformed session token")
	errSessionSignature = errors.New("session signature invalid")
	errSessionExpired   = errors.New("session expired")
	errSessionEmpty     = errors.New("empty session token")
)

// signSession mints a compact JWT for the given access key. now is the
// issued-at timestamp; exp = now + sessionTTL.
func signSession(secret []byte, sub string, now time.Time) (string, sessionClaims, error) {
	if len(secret) == 0 {
		return "", sessionClaims{}, errors.New("jwt secret unset")
	}
	c := sessionClaims{Sub: sub, Iat: now.Unix(), Exp: now.Add(sessionTTL).Unix()}
	header := []byte(`{"alg":"HS256","typ":"JWT"}`)
	cb, err := json.Marshal(c)
	if err != nil {
		return "", sessionClaims{}, err
	}
	signing := b64url(header) + "." + b64url(cb)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(signing))
	return signing + "." + b64url(mac.Sum(nil)), c, nil
}

// verifySession parses and verifies a compact JWT against secret. Returns
// the decoded claims when the signature matches and the token has not yet
// expired (clock-skew tolerance: 0 — sessions are 24h, the boundary is fine).
func verifySession(secret []byte, token string) (*sessionClaims, error) {
	if token == "" {
		return nil, errSessionEmpty
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errSessionMalformed
	}
	signing := parts[0] + "." + parts[1]
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(signing))
	want := mac.Sum(nil)
	got, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, errSessionMalformed
	}
	if !hmac.Equal(want, got) {
		return nil, errSessionSignature
	}
	cb, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, errSessionMalformed
	}
	var c sessionClaims
	if err := json.Unmarshal(cb, &c); err != nil {
		return nil, errSessionMalformed
	}
	if time.Now().Unix() >= c.Exp {
		return nil, errSessionExpired
	}
	return &c, nil
}

func b64url(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

// GenerateSecret returns 32 random bytes. Callers may hex-encode them for
// embedding in env vars / config files.
func GenerateSecret() ([]byte, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return b, nil
}

// DecodeSecret accepts a STRATA_CONSOLE_JWT_SECRET value: hex (32 bytes →
// 64 chars) is decoded, anything else is taken as raw bytes.
func DecodeSecret(s string) []byte {
	if b, err := hex.DecodeString(s); err == nil && len(b) >= 16 {
		return b
	}
	return []byte(s)
}
