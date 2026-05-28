package adminapi

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/danchupin/strata/internal/auth"
)

// loginRequest is the JSON body posted to POST /admin/v1/auth/login.
type loginRequest struct {
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
}

// sessionResponse is the body returned from /auth/login + /auth/whoami.
type sessionResponse struct {
	AccessKey string `json:"access_key"`
	ExpiresAt int64  `json:"expires_at"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	jwtSecret := s.jwtSecret()
	if len(jwtSecret) == 0 {
		writeJSONError(w, http.StatusInternalServerError, "InternalError", "session signing key unconfigured")
		return
	}
	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "invalid JSON body")
		return
	}
	if req.AccessKey == "" || req.SecretKey == "" {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "access_key and secret_key required")
		return
	}
	cred, err := s.Creds.Lookup(r.Context(), req.AccessKey)
	if err != nil || cred == nil {
		// Constant-time fake compare to avoid leaking access-key existence.
		_ = subtle.ConstantTimeCompare([]byte(req.SecretKey), []byte("00000000000000000000000000000000"))
		writeJSONError(w, http.StatusUnauthorized, "InvalidCredentials", "invalid access key or secret")
		return
	}
	if subtle.ConstantTimeCompare([]byte(cred.Secret), []byte(req.SecretKey)) != 1 {
		writeJSONError(w, http.StatusUnauthorized, "InvalidCredentials", "invalid access key or secret")
		return
	}
	tok, claims, err := signSession(jwtSecret, cred.AccessKey, time.Now())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "InternalError", "issue session token")
		return
	}
	http.SetCookie(w, s.sessionCookie(r, tok, int(sessionTTL.Seconds())))
	writeJSON(w, http.StatusOK, sessionResponse{AccessKey: cred.AccessKey, ExpiresAt: claims.Exp})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, s.sessionCookie(r, "", -1))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleWhoami(w http.ResponseWriter, r *http.Request) {
	info := auth.FromContext(r.Context())
	if info == nil || info.IsAnonymous || info.AccessKey == "" {
		writeAuthDenied(w, r, errors.New("no active session"))
		return
	}
	exp := int64(0)
	if c, err := r.Cookie(sessionCookieName); err == nil {
		if claims, err := verifySession(s.jwtSecret(), c.Value); err == nil {
			exp = claims.Exp
		}
	}
	writeJSON(w, http.StatusOK, sessionResponse{AccessKey: info.AccessKey, ExpiresAt: exp})
}

// sessionCookie builds the Set-Cookie value per the PRD spec. The Secure
// flag is set only when the request was received over TLS so dev (`make
// run-memory` over plain HTTP) keeps working. `X-Forwarded-Proto: https`
// is honored only when the request source matches a configured
// STRATA_TRUSTED_PROXIES CIDR (US-007 harden-gateway).
func (s *Server) sessionCookie(r *http.Request, value string, maxAge int) *http.Cookie {
	// #nosec G124: Secure flag set conditionally via s.isHTTPS(r) so plain-HTTP dev keeps working; HttpOnly + SameSite=Strict explicit.
	return &http.Cookie{
		Name:     sessionCookieName,
		Value:    value,
		Path:     "/admin",
		HttpOnly: true,
		Secure:   s.isHTTPS(r),
		SameSite: http.SameSiteStrictMode,
		MaxAge:   maxAge,
	}
}

// isHTTPS reports whether the request was received over TLS, optionally
// honoring X-Forwarded-Proto when the source matches a trusted-proxy
// CIDR. Default-empty STRATA_TRUSTED_PROXIES = forwarded header ignored.
func (s *Server) isHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return s.TrustedProxies.ForwardedProto(r) == "https"
}
