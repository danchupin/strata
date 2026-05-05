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

	"github.com/danchupin/strata/internal/auditstream"
	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/heartbeat"
	"github.com/danchupin/strata/internal/leader"
	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/otel/ringbuf"
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
	// jwtMu guards JWTSecret + JWTEphemeral. Reads in the auth path use
	// jwtSecret() / jwtEphemeral(); writes via SetJWTSecret() (US-019
	// Rotate JWT secret).
	jwtMu        sync.RWMutex
	JWTSecret    []byte
	JWTEphemeral bool
	// JWTSecretFile is the on-disk path used by SetJWTSecret to persist a
	// freshly-rotated key. Empty disables the rotate endpoint with 503
	// SecretFileUnavailable so dev rigs without a writable path fail loud.
	JWTSecretFile string
	// PrometheusURL surfaces on GET /admin/v1/settings (US-019); empty
	// when STRATA_PROMETHEUS_URL is unset.
	PrometheusURL string
	// OtelEndpoint mirrors OTEL_EXPORTER_OTLP_ENDPOINT and surfaces on
	// GET /admin/v1/cluster/status (US-006) so the trace browser can render
	// the "Open in Jaeger" deep link only when an OTLP collector is wired.
	OtelEndpoint string
	// HeartbeatInterval echoes heartbeat.DefaultInterval into the Settings
	// view so operators can verify the cadence without grepping the source.
	HeartbeatInterval time.Duration
	// ConsoleThemeDefault is "system" today; reserved for future
	// operator-set defaults.
	ConsoleThemeDefault string
	// CassandraSettings / RADOSSettings / TiKVSettings carry per-backend
	// connection parameters surfaced (read-only) on the Settings →
	// Backends tab. Populated only for the currently-selected backend.
	CassandraSettings CassandraSettings
	RADOSSettings     RADOSSettings
	TiKVSettings      TiKVSettings
	// S3Backend exposes s3-over-s3 backend config on /admin/v1/settings/
	// data-backend (US-021). Populated when DataBackend == "s3".
	S3Backend S3BackendSettings
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

	// AuditStream is the in-process pub-sub fan-out backing
	// GET /admin/v1/audit/stream (US-001). Wired by serverapp; nil disables
	// the SSE endpoint with 503 Unavailable.
	AuditStream *auditstream.Broadcaster
	// AuditStreamKeepAliveInterval overrides the default 25s SSE keep-alive
	// ping cadence. Set by tests; production uses the default.
	AuditStreamKeepAliveInterval time.Duration

	// TraceRingbuf is the in-process OTel trace ring buffer (US-005)
	// backing GET /admin/v1/diagnostics/trace/{requestID}. nil disables
	// the endpoint with 503 RingbufUnavailable.
	TraceRingbuf *ringbuf.RingBuffer

	// hotBucketsMu guards lazy initialisation of hotBucketsCacheVal — the
	// 30s TTL cache that absorbs burst polls of /admin/v1/diagnostics/
	// hot-buckets (US-007).
	hotBucketsMu       sync.Mutex
	hotBucketsCacheVal *hotBucketsCache

	// hotShardsMu guards lazy initialisation of hotShardsCacheVal — the
	// 30s TTL cache for /admin/v1/diagnostics/hot-shards/{bucket} (US-009).
	hotShardsMu       sync.Mutex
	hotShardsCacheVal *hotShardsCache

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
	JWTSecret    []byte
	JWTEphemeral bool
	// JWTSecretFile is the persistence path used by POST /admin/v1/settings/
	// jwt/rotate. Empty falls back to /etc/strata/jwt-secret.
	JWTSecretFile string
	// PrometheusURL is echoed into GET /admin/v1/settings (US-019).
	PrometheusURL string
	// OtelEndpoint mirrors OTEL_EXPORTER_OTLP_ENDPOINT — surfaced on
	// GET /admin/v1/cluster/status (US-006) so the trace browser UI can
	// gate its "Open in Jaeger" link.
	OtelEndpoint string
	// HeartbeatInterval mirrors heartbeat.DefaultInterval; surfaced on the
	// Settings page Cluster tab.
	HeartbeatInterval time.Duration
	// ConsoleThemeDefault is the default theme reported on the Console tab.
	ConsoleThemeDefault string
	// CassandraSettings / RADOSSettings / TiKVSettings carry per-backend
	// connection parameters surfaced (read-only) on the Settings →
	// Backends tab.
	CassandraSettings CassandraSettings
	RADOSSettings     RADOSSettings
	TiKVSettings      TiKVSettings
	// S3Backend exposes s3-over-s3 backend config on /admin/v1/settings/
	// data-backend (US-021). Populated when DataBackend == "s3".
	S3Backend S3BackendSettings
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
	// AuditStream is the live audit-tail broadcaster passed in by serverapp;
	// the same handle is given to s3api.AuditMiddleware as the publisher.
	AuditStream *auditstream.Broadcaster
	// TraceRingbuf is the in-process OTel trace ring buffer (US-005). nil
	// disables the trace browser endpoint with 503 RingbufUnavailable.
	TraceRingbuf *ringbuf.RingBuffer
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
		Meta:                 c.Meta,
		Creds:                c.Creds,
		Heartbeat:            c.Heartbeat,
		Prom:                 c.Prom,
		Locker:               c.Locker,
		Version:              c.Version,
		ClusterName:          clusterName,
		Region:               c.Region,
		MetaBackend:          c.MetaBackend,
		DataBackend:          c.DataBackend,
		Started:              time.Now(),
		JWTSecret:            c.JWTSecret,
		JWTEphemeral:         c.JWTEphemeral,
		JWTSecretFile:        c.JWTSecretFile,
		PrometheusURL:        c.PrometheusURL,
		OtelEndpoint:         c.OtelEndpoint,
		HeartbeatInterval:    c.HeartbeatInterval,
		ConsoleThemeDefault:  c.ConsoleThemeDefault,
		CassandraSettings:    c.CassandraSettings,
		RADOSSettings:        c.RADOSSettings,
		TiKVSettings:         c.TiKVSettings,
		S3Backend:            c.S3Backend,
		AuditTTL:             c.AuditTTL,
		InvalidateCredential: c.InvalidateCredential,
		S3Handler:            c.S3Handler,
		AuditStream:          c.AuditStream,
		TraceRingbuf:         c.TraceRingbuf,
		Logger:               log.Default(),
	}
}

// jwtSecret returns the active session-signing key. Goroutine-safe; rotate
// via SetJWTSecret. Used by authMiddleware + handleLogin / handleWhoami.
func (s *Server) jwtSecret() []byte {
	s.jwtMu.RLock()
	defer s.jwtMu.RUnlock()
	return s.JWTSecret
}

// jwtEphemeral reports whether the active key was generated at startup.
// Used only on GET /admin/v1/settings to surface the operator-facing source
// label.
func (s *Server) jwtEphemeral() bool {
	s.jwtMu.RLock()
	defer s.jwtMu.RUnlock()
	return s.JWTEphemeral
}

// SetJWTSecret atomically replaces the in-memory session-signing key. The
// in-flight cookie state on the operator's browser is implicitly invalidated:
// every cookie signed by the previous key now fails verifySession with
// ErrSessionSignature, so the next request returns 401 and the UI reroutes
// to /login. Wired by handleRotateJWTSecret (US-019).
func (s *Server) SetJWTSecret(b []byte) {
	s.jwtMu.Lock()
	s.JWTSecret = b
	s.JWTEphemeral = false
	s.jwtMu.Unlock()
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
	mux.HandleFunc("PUT /admin/v1/buckets/{bucket}/backend-presign", s.handleBucketSetBackendPresign)
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
	mux.HandleFunc("GET /admin/v1/buckets/{bucket}/distribution", s.handleBucketDistribution)
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
	mux.HandleFunc("GET /admin/v1/audit/stream", s.handleAuditStream)
	mux.HandleFunc("GET /admin/v1/diagnostics/slow-queries", s.handleDiagnosticsSlowQueries)
	mux.HandleFunc("GET /admin/v1/diagnostics/trace/{requestID}", s.handleDiagnosticsTrace)
	mux.HandleFunc("GET /admin/v1/diagnostics/hot-buckets", s.handleDiagnosticsHotBuckets)
	mux.HandleFunc("GET /admin/v1/diagnostics/hot-shards/{bucket}", s.handleDiagnosticsHotShards)
	mux.HandleFunc("GET /admin/v1/diagnostics/node/{nodeID}", s.handleDiagnosticsNode)
	mux.HandleFunc("GET /admin/v1/settings", s.handleGetSettings)
	mux.HandleFunc("GET /admin/v1/settings/data-backend", s.handleGetSettingsDataBackend)
	mux.HandleFunc("POST /admin/v1/settings/jwt/rotate", s.handleRotateJWTSecret)
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
			claims, vErr := verifySession(s.jwtSecret(), c.Value)
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
