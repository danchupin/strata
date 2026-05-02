// Package adminapi serves the JSON HTTP namespace at /admin/v1/* used by the
// embedded operator console. Phase 1 (US-003) wires stub handlers; later
// stories fill in real data.
//
// All endpoints return application/json. Authentication uses the same SigV4
// path as the S3 API (US-004 layers a session cookie + JWT on top). Anonymous
// requests get a 401 JSON error.
package adminapi

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/meta"
)

// Server holds dependencies the /admin/v1/* handlers need.
type Server struct {
	Meta    meta.Store
	Creds   auth.CredentialsStore
	Version string
	Started time.Time
	Logger  *log.Logger
}

// New constructs a Server with the given dependencies. Started defaults to now.
func New(m meta.Store, creds auth.CredentialsStore, version string) *Server {
	return &Server{
		Meta:    m,
		Creds:   creds,
		Version: version,
		Started: time.Now(),
		Logger:  log.Default(),
	}
}

// Handler returns the auth-wrapped HTTP handler suitable for mounting under
// /admin/v1/ on the gateway mux.
func (s *Server) Handler() http.Handler {
	mw := &auth.Middleware{Store: s.Creds, Mode: auth.ModeRequired}
	return mw.Wrap(s.routes(), writeAuthDenied)
}

// routes builds the inner mux without authentication — exposed only to tests
// in this package so handler shape can be asserted independently of SigV4.
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/v1/cluster/status", s.handleClusterStatus)
	mux.HandleFunc("GET /admin/v1/cluster/nodes", s.handleClusterNodes)
	mux.HandleFunc("GET /admin/v1/buckets", s.handleBucketsList)
	mux.HandleFunc("GET /admin/v1/buckets/top", s.handleBucketsTop)
	mux.HandleFunc("GET /admin/v1/buckets/{bucket}", s.handleBucketGet)
	mux.HandleFunc("GET /admin/v1/buckets/{bucket}/objects", s.handleObjectsList)
	mux.HandleFunc("GET /admin/v1/consumers/top", s.handleConsumersTop)
	mux.HandleFunc("GET /admin/v1/metrics/timeseries", s.handleMetricsTimeseries)
	return mux
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
