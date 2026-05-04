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
	"sync"
	"time"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/heartbeat"
	"github.com/danchupin/strata/internal/leader"
	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/promclient"
)

// Server holds dependencies the /admin/v1/* handlers need.
type Server struct {
	Meta        meta.Store
	Creds       auth.CredentialsStore
	Heartbeat   heartbeat.Store
	Prom        *promclient.Client
	Locker      leader.Locker
	Version     string
	ClusterName string
	Region      string
	MetaBackend string
	DataBackend string
	Started     time.Time
	JWTSecret   []byte
	// AuditTTL is the row TTL applied when an admin handler writes an extra
	// audit_log row directly via meta.Store.EnqueueAudit (e.g. the
	// DeleteIAMUser cascade in US-011 emits one admin:DeleteAccessKey row
	// per cascaded key in addition to the request-scoped admin:DeleteUser
	// row stamped by the AuditMiddleware override). Zero falls back to
	// s3api.DefaultAuditRetention.
	AuditTTL time.Duration
	// InvalidateCredential, when set, drops a cached credential lookup so
	// the next SigV4 verification re-fetches the underlying record from
	// meta. Wired by serverapp to auth.MultiStore.Invalidate; admin handlers
	// that flip Disabled / Delete on an access key call this so an
	// in-flight cache hit cannot keep a freshly-disabled key alive past the
	// auth.DefaultCacheTTL window.
	InvalidateCredential func(accessKey string)
	// S3Handler, when set, is the gateway's S3 router (s3api.Server). Admin
	// upload handlers (US-015) forward CompleteMultipartUpload + AbortMultipart
	// through it so chunk cleanup / etag composition / replication / notify
	// stay consistent with the gateway-side multipart path. Set by serverapp;
	// nil disables the upload-complete + upload-abort endpoints.
	S3Handler http.Handler
	Logger    *log.Logger

	// jobsMu guards background-job goroutines. Currently only the
	// force-empty drain (US-002) registers here; we cancel the goroutine
	// on Server shutdown so a graceful drain doesn't outlive the gateway.
	jobsMu sync.Mutex
	jobsWG sync.WaitGroup
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
	// Locker carries the meta-backed leader-election locker so admin-side
	// background jobs (force-empty, US-002) can claim a per-bucket lease
	// in worker_locks before kicking their goroutine. nil → memory or
	// unsupported backend; the force-empty endpoint fails closed.
	Locker      leader.Locker
	Version     string
	ClusterName string
	Region      string
	MetaBackend string
	DataBackend string
	JWTSecret   []byte
	// AuditTTL mirrors the gateway's STRATA_AUDIT_RETENTION-derived value so
	// admin handlers that write extra audit rows (cascaded key deletes,
	// future bulk operations) match the row TTL of the AuditMiddleware.
	AuditTTL time.Duration
	// InvalidateCredential is the auth.MultiStore.Invalidate hook so admin
	// access-key disable/delete handlers can drop the cache entry the
	// gateway holds for the affected access key.
	InvalidateCredential func(accessKey string)
	// S3Handler is the gateway's S3 router (s3api.Server). Admin upload
	// Complete / Abort handlers forward through it so the existing
	// multipart finalisation logic stays the single source of truth.
	S3Handler http.Handler
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
		Locker:      c.Locker,
		Version:     c.Version,
		ClusterName: clusterName,
		Region:      c.Region,
		MetaBackend: c.MetaBackend,
		DataBackend: c.DataBackend,
		Started:     time.Now(),
		JWTSecret:   c.JWTSecret,
		AuditTTL:             c.AuditTTL,
		InvalidateCredential: c.InvalidateCredential,
		S3Handler:            c.S3Handler,
		Logger:               log.Default(),
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
	mux.HandleFunc("DELETE /admin/v1/buckets/{bucket}", s.handleBucketDelete)
	mux.HandleFunc("POST /admin/v1/buckets/{bucket}/force-empty", s.handleBucketForceEmpty)
	mux.HandleFunc("GET /admin/v1/buckets/{bucket}/force-empty/{jobID}", s.handleBucketForceEmptyStatus)
	mux.HandleFunc("PUT /admin/v1/buckets/{bucket}/versioning", s.handleBucketSetVersioning)
	mux.HandleFunc("GET /admin/v1/buckets/{bucket}/object-lock", s.handleBucketGetObjectLock)
	mux.HandleFunc("PUT /admin/v1/buckets/{bucket}/object-lock", s.handleBucketSetObjectLock)
	mux.HandleFunc("GET /admin/v1/buckets/{bucket}/lifecycle", s.handleBucketGetLifecycle)
	mux.HandleFunc("PUT /admin/v1/buckets/{bucket}/lifecycle", s.handleBucketSetLifecycle)
	mux.HandleFunc("GET /admin/v1/buckets/{bucket}/cors", s.handleBucketGetCORS)
	mux.HandleFunc("PUT /admin/v1/buckets/{bucket}/cors", s.handleBucketSetCORS)
	mux.HandleFunc("DELETE /admin/v1/buckets/{bucket}/cors", s.handleBucketDeleteCORS)
	mux.HandleFunc("GET /admin/v1/buckets/{bucket}/policy", s.handleBucketGetPolicy)
	mux.HandleFunc("PUT /admin/v1/buckets/{bucket}/policy", s.handleBucketSetPolicy)
	mux.HandleFunc("DELETE /admin/v1/buckets/{bucket}/policy", s.handleBucketDeletePolicy)
	mux.HandleFunc("POST /admin/v1/buckets/{bucket}/policy/dry-run", s.handleBucketDryRunPolicy)
	mux.HandleFunc("GET /admin/v1/buckets/{bucket}/acl", s.handleBucketGetACL)
	mux.HandleFunc("PUT /admin/v1/buckets/{bucket}/acl", s.handleBucketSetACL)
	mux.HandleFunc("GET /admin/v1/buckets/{bucket}/inventory", s.handleBucketListInventory)
	mux.HandleFunc("PUT /admin/v1/buckets/{bucket}/inventory/{configID}", s.handleBucketSetInventory)
	mux.HandleFunc("DELETE /admin/v1/buckets/{bucket}/inventory/{configID}", s.handleBucketDeleteInventory)
	mux.HandleFunc("GET /admin/v1/buckets/{bucket}/logging", s.handleBucketGetLogging)
	mux.HandleFunc("PUT /admin/v1/buckets/{bucket}/logging", s.handleBucketSetLogging)
	mux.HandleFunc("DELETE /admin/v1/buckets/{bucket}/logging", s.handleBucketDeleteLogging)
	mux.HandleFunc("GET /admin/v1/buckets/top", s.handleBucketsTop)
	mux.HandleFunc("GET /admin/v1/buckets/{bucket}", s.handleBucketGet)
	mux.HandleFunc("GET /admin/v1/buckets/{bucket}/objects", s.handleObjectsList)
	mux.HandleFunc("GET /admin/v1/buckets/{bucket}/object", s.handleObjectGet)
	mux.HandleFunc("GET /admin/v1/buckets/{bucket}/object-versions", s.handleObjectVersions)
	// Deviation from US-016 AC: the AC names PUT /admin/v1/buckets/{bucket}/
	// objects/{key}/tags etc., but Go 1.22 mux trailing wildcards must be the
	// LAST segment of the pattern — `{key...}/tags` is rejected at register
	// time. The DELETE shape keeps {key...} (no subroute follows) so the AC
	// URL works there; tags / retention / legal-hold collapse to per-shape
	// endpoints with the key carried in the JSON body. Same trick used in
	// US-015 single-presign and US-013 / US-014 ARN routes.
	mux.HandleFunc("PUT /admin/v1/buckets/{bucket}/object-tags", s.handleObjectTags)
	mux.HandleFunc("PUT /admin/v1/buckets/{bucket}/object-retention", s.handleObjectRetention)
	mux.HandleFunc("PUT /admin/v1/buckets/{bucket}/object-legal-hold", s.handleObjectLegalHold)
	mux.HandleFunc("DELETE /admin/v1/buckets/{bucket}/objects/{key...}", s.handleObjectDelete)
	mux.HandleFunc("POST /admin/v1/buckets/{bucket}/uploads", s.handleUploadInit)
	mux.HandleFunc("POST /admin/v1/buckets/{bucket}/uploads/{uploadID}/parts/{partNumber}/presign", s.handleUploadPartPresign)
	mux.HandleFunc("POST /admin/v1/buckets/{bucket}/uploads/{uploadID}/complete", s.handleUploadComplete)
	mux.HandleFunc("DELETE /admin/v1/buckets/{bucket}/uploads/{uploadID}", s.handleUploadAbort)
	// Deviation from US-015 AC: AC names POST /admin/v1/buckets/{bucket}/objects/
	// {key}/single-presign, but Go 1.22 mux trailing wildcards must be at the
	// END of the pattern — `{key...}/single-presign` is rejected at register
	// time. Collapsed to POST /admin/v1/buckets/{bucket}/single-presign with
	// the key carried in the JSON body. Same trick used in US-013 / US-014
	// for ARN routes.
	mux.HandleFunc("POST /admin/v1/buckets/{bucket}/single-presign", s.handleSinglePutPresign)
	mux.HandleFunc("GET /admin/v1/iam/users", s.handleIAMUsersList)
	mux.HandleFunc("POST /admin/v1/iam/users", s.handleIAMUserCreate)
	mux.HandleFunc("DELETE /admin/v1/iam/users/{userName}", s.handleIAMUserDelete)
	mux.HandleFunc("GET /admin/v1/iam/users/{userName}/access-keys", s.handleIAMAccessKeyList)
	mux.HandleFunc("POST /admin/v1/iam/users/{userName}/access-keys", s.handleIAMAccessKeyCreate)
	mux.HandleFunc("PATCH /admin/v1/iam/access-keys/{accessKey}", s.handleIAMAccessKeyUpdate)
	mux.HandleFunc("DELETE /admin/v1/iam/access-keys/{accessKey}", s.handleIAMAccessKeyDelete)
	mux.HandleFunc("GET /admin/v1/iam/policies", s.handleIAMPoliciesList)
	mux.HandleFunc("POST /admin/v1/iam/policies", s.handleIAMPolicyCreate)
	mux.HandleFunc("PUT /admin/v1/iam/policies/{arn...}", s.handleIAMPolicyUpdate)
	mux.HandleFunc("DELETE /admin/v1/iam/policies/{arn...}", s.handleIAMPolicyDelete)
	mux.HandleFunc("GET /admin/v1/iam/users/{userName}/policies", s.handleIAMUserPoliciesList)
	mux.HandleFunc("POST /admin/v1/iam/users/{userName}/policies", s.handleIAMUserPolicyAttach)
	mux.HandleFunc("DELETE /admin/v1/iam/users/{userName}/policies/{policyArn...}", s.handleIAMUserPolicyDetach)
	mux.HandleFunc("GET /admin/v1/multipart/active", s.handleMultipartActive)
	mux.HandleFunc("POST /admin/v1/multipart/abort", s.handleMultipartAbort)
	mux.HandleFunc("GET /admin/v1/audit", s.handleAuditList)
	mux.HandleFunc("GET /admin/v1/audit.csv", s.handleAuditCSV)
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
