package s3api

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/logging"
	"github.com/danchupin/strata/internal/meta"
)

// AuditOverride lets a downstream handler (e.g. /admin/v1/* under adminapi)
// stamp the action/resource/principal it wants AuditMiddleware to record
// instead of the path-derived defaults. AuditMiddleware installs a fresh
// pointer in the request context before calling Next; the inner handler
// mutates the same struct via SetAuditOverride, and AuditMiddleware reads
// the result on the way back out. Action="" means "no override; fall back
// to path-derived audit row".
type AuditOverride struct {
	Action    string
	Resource  string
	Bucket    string
	Principal string
}

type auditOverrideKey struct{}

// withAuditOverride attaches a fresh AuditOverride pointer to ctx. The
// pointer is shared with the inner handler so SetAuditOverride mutations
// are visible to the audit middleware after Next returns.
func withAuditOverride(ctx context.Context) (context.Context, *AuditOverride) {
	o := &AuditOverride{}
	return context.WithValue(ctx, auditOverrideKey{}, o), o
}

// SetAuditOverride records the audit action/resource/bucket/principal that
// AuditMiddleware should emit for the current request. Bucket is optional
// and used to resolve the row's BucketID via meta.Store.GetBucket; pass ""
// for non-bucket-scoped actions. Calls outside an AuditMiddleware-wrapped
// chain are no-ops.
func SetAuditOverride(ctx context.Context, action, resource, bucket, principal string) {
	if ov, ok := ctx.Value(auditOverrideKey{}).(*AuditOverride); ok {
		ov.Action = action
		ov.Resource = resource
		ov.Bucket = bucket
		ov.Principal = principal
	}
}

// DefaultAuditRetention is the row TTL applied when STRATA_AUDIT_RETENTION
// is unset or empty.
const DefaultAuditRetention = 30 * 24 * time.Hour

// ParseAuditRetention parses STRATA_AUDIT_RETENTION-style values. Empty input
// returns DefaultAuditRetention. Bare-integer suffix "d" is accepted as days.
func ParseAuditRetention(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return DefaultAuditRetention, nil
	}
	if strings.HasSuffix(s, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err != nil || n < 0 {
			return 0, err
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

// AuditMiddleware appends one audit_log row per state-changing HTTP request
// (US-022). Read-only requests (GET/HEAD) and OPTIONS preflight are skipped.
// The row carries the request_id installed by logging.Middleware so audit and
// access logs correlate. Best-effort: meta failures are swallowed so a flaky
// audit path never fails the underlying request.
type AuditMiddleware struct {
	Meta meta.Store
	Next http.Handler
	TTL  time.Duration
	Now  func() time.Time
}

// NewAuditMiddleware wraps next so each state-changing request is appended to
// audit_log with the configured row TTL.
func NewAuditMiddleware(store meta.Store, ttl time.Duration, next http.Handler) *AuditMiddleware {
	return &AuditMiddleware{Meta: store, Next: next, TTL: ttl}
}

func (m *AuditMiddleware) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	now := m.Now
	if now == nil {
		now = time.Now
	}
	rw := &auditWriter{ResponseWriter: w, status: http.StatusOK}
	innerCtx, ov := withAuditOverride(r.Context())
	m.Next.ServeHTTP(rw, r.WithContext(innerCtx))

	if !auditableMethod(r.Method) {
		return
	}
	if ov.Action != "" {
		m.recordOverride(r, rw, ov, now)
		return
	}
	bucket, key := splitPath(r.URL.Path)
	q := r.URL.Query()
	iamAction := extractIAMAction(r)
	if bucket == "" && iamAction == "" {
		// Anonymous probe / unrecognised root path; nothing useful to log.
		return
	}
	ctx := r.Context()
	var bucketID uuid.UUID
	storedBucket := bucket
	if bucket != "" {
		if b, err := m.Meta.GetBucket(ctx, bucket); err == nil {
			bucketID = b.ID
		}
	}
	if storedBucket == "" {
		storedBucket = "-"
	}
	entry := &meta.AuditEvent{
		BucketID:  bucketID,
		Bucket:    storedBucket,
		Time:      now().UTC(),
		Principal: principalFromContext(r),
		Action:    deriveAuditAction(r.Method, bucket, key, q, iamAction),
		Resource:  deriveAuditResource(bucket, key, iamAction),
		Result:    strconv.Itoa(rw.status),
		RequestID: logging.RequestIDFromContext(ctx),
		SourceIP:  clientSourceIP(r),
		UserAgent: r.UserAgent(),
	}
	if entry.RequestID == "" {
		entry.RequestID = r.Header.Get(logging.HeaderRequestID)
	}
	_ = m.Meta.EnqueueAudit(ctx, entry, m.TTL)
}

// recordOverride emits an audit row built from the AuditOverride that the
// inner handler stamped onto the request context. Path-derived bucket/key
// inspection is skipped because admin endpoints sit at /admin/v1/* and the
// resource string the operator cares about is supplied verbatim by the
// handler.
func (m *AuditMiddleware) recordOverride(r *http.Request, rw *auditWriter, ov *AuditOverride, now func() time.Time) {
	ctx := r.Context()
	var bucketID uuid.UUID
	storedBucket := ov.Bucket
	if storedBucket != "" && m.Meta != nil {
		if b, err := m.Meta.GetBucket(ctx, storedBucket); err == nil {
			bucketID = b.ID
		}
	}
	if storedBucket == "" {
		storedBucket = "-"
	}
	entry := &meta.AuditEvent{
		BucketID:  bucketID,
		Bucket:    storedBucket,
		Time:      now().UTC(),
		Principal: ov.Principal,
		Action:    ov.Action,
		Resource:  ov.Resource,
		Result:    strconv.Itoa(rw.status),
		RequestID: logging.RequestIDFromContext(ctx),
		SourceIP:  clientSourceIP(r),
		UserAgent: r.UserAgent(),
	}
	if entry.RequestID == "" {
		entry.RequestID = r.Header.Get(logging.HeaderRequestID)
	}
	_ = m.Meta.EnqueueAudit(ctx, entry, m.TTL)
}

func auditableMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodConnect, http.MethodTrace:
		return false
	}
	return true
}

func deriveAuditAction(method, bucket, key string, q url.Values, iamAction string) string {
	if iamAction != "" {
		return iamAction
	}
	if bucket == "" {
		return method
	}
	scope := "Bucket"
	if key != "" {
		scope = "Object"
	}
	for _, sub := range auditSubresources {
		if !q.Has(sub) {
			continue
		}
		return method + scope + auditSubresourceLabel(sub)
	}
	switch method {
	case http.MethodPut:
		if scope == "Bucket" {
			return "CreateBucket"
		}
		return "PutObject"
	case http.MethodDelete:
		if scope == "Bucket" {
			return "DeleteBucket"
		}
		return "DeleteObject"
	case http.MethodPost:
		return "Post" + scope
	}
	return method + scope
}

// auditSubresources tracks query-string sub-resources we want a friendlier
// action label for. Mirrors the access-log shape but emits CamelCase action
// names matching the AWS S3 API verbs.
var auditSubresources = []string{
	"acl", "policy", "lifecycle", "cors", "tagging", "logging", "website",
	"versioning", "encryption", "object-lock", "publicAccessBlock",
	"ownershipControls", "notification", "replication", "uploads", "uploadId",
	"requestPayment", "accelerate", "delete", "restore", "retention",
	"legal-hold",
}

func auditSubresourceLabel(sub string) string {
	switch sub {
	case "acl":
		return "Acl"
	case "policy":
		return "Policy"
	case "lifecycle":
		return "Lifecycle"
	case "cors":
		return "Cors"
	case "tagging":
		return "Tagging"
	case "logging":
		return "Logging"
	case "website":
		return "Website"
	case "versioning":
		return "Versioning"
	case "encryption":
		return "Encryption"
	case "object-lock":
		return "ObjectLockConfig"
	case "publicAccessBlock":
		return "PublicAccessBlock"
	case "ownershipControls":
		return "OwnershipControls"
	case "notification":
		return "NotificationConfig"
	case "replication":
		return "Replication"
	case "uploads":
		return "Uploads"
	case "uploadId":
		return "UploadPart"
	case "requestPayment":
		return "RequestPayment"
	case "accelerate":
		return "Accelerate"
	case "delete":
		return "Delete"
	case "restore":
		return "Restore"
	case "retention":
		return "Retention"
	case "legal-hold":
		return "LegalHold"
	}
	return sub
}

func deriveAuditResource(bucket, key, iamAction string) string {
	if bucket == "" {
		if iamAction != "" {
			return "iam:" + iamAction
		}
		return ""
	}
	if key == "" {
		return "/" + bucket
	}
	return "/" + bucket + "/" + key
}

type auditWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *auditWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.status = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *auditWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.wroteHeader = true
	}
	return w.ResponseWriter.Write(p)
}
