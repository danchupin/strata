// Package adminapi serves the JSON HTTP namespace at /admin/v1/* used by the
// embedded operator console. Phase 1 (US-003) wires stub handlers; later
// stories fill in real data.
//
// All endpoints return application/json. Authentication: a session cookie
// (strata_session=<HS256 JWT>, US-004) is preferred for browser traffic; the
// SigV4 path is the fallback for CLI clients. Anonymous requests get a 401
// JSON error.
package adminapi

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/heartbeat"
	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/promclient"
)

// Server holds dependencies the /admin/v1/* handlers need.
type Server struct {
	Meta        meta.Store
	Creds       auth.CredentialsStore
	Heartbeat   heartbeat.Store
	Prom        *promclient.Client
	Version     string
	ClusterName string
	Region      string
	MetaBackend string
	DataBackend string
	Started     time.Time
	JWTSecret   []byte
	Logger      *log.Logger
}

// Config carries everything New needs to build a Server. Required fields:
// Creds + JWTSecret. Heartbeat may be nil — the cluster overview will then
// surface only the local replica derived from Started/Version. Backend names
// echo into ClusterStatus.{meta,data}_backend; leave empty to omit. Region
// echoes into BucketSummary.region — Strata is single-region today, so every
// bucket reports the gateway's configured RegionName.
type Config struct {
	Meta        meta.Store
	Creds       auth.CredentialsStore
	Heartbeat   heartbeat.Store
	Prom        *promclient.Client
	Version     string
	ClusterName string
	Region      string
	MetaBackend string
	DataBackend string
	JWTSecret   []byte
}

// New constructs a Server. Started defaults to now. JWTSecret empty means
// login fails closed (gateway logs a WARN at startup if env unset and
// generates an ephemeral secret). ClusterName falls back to "strata".
func New(c Config) *Server {
	clusterName := c.ClusterName
	if clusterName == "" {
		clusterName = "strata"
	}
	return &Server{
		Meta:        c.Meta,
		Creds:       c.Creds,
		Heartbeat:   c.Heartbeat,
		Prom:        c.Prom,
		Version:     c.Version,
		ClusterName: clusterName,
		Region:      c.Region,
		MetaBackend: c.MetaBackend,
		DataBackend: c.DataBackend,
		Started:     time.Now(),
		JWTSecret:   c.JWTSecret,
		Logger:      log.Default(),
	}
}

// Handler returns the auth-wrapped HTTP handler suitable for mounting under
// /admin/v1/ on the gateway mux. Auth resolution order per request:
//
//  1. Path is /admin/v1/auth/login or /admin/v1/auth/logout → no auth.
//  2. strata_session cookie present → verify JWT, set AuthInfo, serve.
//  3. Otherwise fall through to SigV4 middleware.
func (s *Server) Handler() http.Handler {
	return s.authMiddleware(s.routes())
}

// routes builds the inner mux without authentication — exposed to tests in
// this package so handler shape can be asserted independently of SigV4.
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /admin/v1/auth/login", s.handleLogin)
	mux.HandleFunc("POST /admin/v1/auth/logout", s.handleLogout)
	mux.HandleFunc("GET /admin/v1/auth/whoami", s.handleWhoami)
	mux.HandleFunc("GET /admin/v1/cluster/status", s.handleClusterStatus)
	mux.HandleFunc("GET /admin/v1/cluster/nodes", s.handleClusterNodes)
	mux.HandleFunc("GET /admin/v1/buckets", s.handleBucketsList)
	mux.HandleFunc("POST /admin/v1/buckets", s.handleBucketCreate)
	mux.HandleFunc("GET /admin/v1/buckets/top", s.handleBucketsTop)
	mux.HandleFunc("GET /admin/v1/buckets/{bucket}", s.handleBucketGet)
	mux.HandleFunc("GET /admin/v1/buckets/{bucket}/objects", s.handleObjectsList)
	mux.HandleFunc("GET /admin/v1/consumers/top", s.handleConsumersTop)
	mux.HandleFunc("GET /admin/v1/metrics/timeseries", s.handleMetricsTimeseries)
	return mux
}

// isAuthBypassPath returns true for paths that must accept anonymous traffic
// (login + logout). Whoami still requires auth so it can act as a "session
// alive?" probe for the UI.
func isAuthBypassPath(path string) bool {
	return path == "/admin/v1/auth/login" || path == "/admin/v1/auth/logout"
}

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	sigv4 := &auth.Middleware{Store: s.Creds, Mode: auth.ModeRequired}
	sigv4Handler := sigv4.Wrap(next, writeAuthDenied)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isAuthBypassPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		if c, err := r.Cookie(sessionCookieName); err == nil && c.Value != "" {
			claims, vErr := verifySession(s.JWTSecret, c.Value)
			if vErr != nil {
				writeAuthDenied(w, r, vErr)
				return
			}
			cred, lErr := s.Creds.Lookup(r.Context(), claims.Sub)
			if lErr != nil || cred == nil {
				writeAuthDenied(w, r, errors.New("session subject not found"))
				return
			}
			ctx := auth.WithAuth(r.Context(), &auth.AuthInfo{
				AccessKey: cred.AccessKey,
				Owner:     cred.Owner,
			})
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		// Authorization header (SigV4) takes precedence over an absent cookie.
		if r.Header.Get("Authorization") == "" && !hasPresignedQuery(r) {
			writeAuthDenied(w, r, auth.ErrMissingSignature)
			return
		}
		sigv4Handler.ServeHTTP(w, r)
	})
}

// hasPresignedQuery returns true when the request carries SigV4 presign
// query parameters. We avoid a full parse here — the auth.Middleware does
// that — but we want to short-circuit obviously-anonymous browser traffic
// without delegating to SigV4 just to get the same 401 back.
func hasPresignedQuery(r *http.Request) bool {
	q := r.URL.RawQuery
	return q != "" && (strings.Contains(q, "X-Amz-Signature=") ||
		strings.Contains(q, "X-Amz-Credential="))
}

// errorResponse is the JSON shape returned for 4xx/5xx errors.
type errorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("adminapi: encode response: %v", err)
	}
}

func writeJSONError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorResponse{Code: code, Message: message})
}

func writeAuthDenied(w http.ResponseWriter, r *http.Request, err error) {
	msg := "authentication required"
	if err != nil {
		msg = err.Error()
	}
	writeJSONError(w, http.StatusUnauthorized, "Unauthorized", msg)
}
