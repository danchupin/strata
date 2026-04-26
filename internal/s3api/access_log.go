package s3api

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/danchupin/strata/internal/meta"
)

// AccessLogMiddleware buffers one server-access-log row per request when the
// target bucket has logging configured (US-013). The strata-access-log worker
// (US-014) drains the buffer into AWS-format log files. The middleware is
// best-effort: meta failures are swallowed so a flaky logging path never
// fails the underlying request.
type AccessLogMiddleware struct {
	Meta meta.Store
	Next http.Handler
	Now  func() time.Time
}

// NewAccessLogMiddleware wraps next so each request that targets a bucket
// with logging enabled is buffered into the access_log_buffer table.
func NewAccessLogMiddleware(store meta.Store, next http.Handler) *AccessLogMiddleware {
	return &AccessLogMiddleware{Meta: store, Next: next}
}

func (m *AccessLogMiddleware) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	now := m.Now
	if now == nil {
		now = time.Now
	}
	start := now()
	rw := &accessLogWriter{ResponseWriter: w, status: http.StatusOK}
	m.Next.ServeHTTP(rw, r)

	bucket, key := splitPath(r.URL.Path)
	if bucket == "" {
		return
	}
	ctx := r.Context()
	b, err := m.Meta.GetBucket(ctx, bucket)
	if err != nil {
		return
	}
	if _, err := m.Meta.GetBucketLogging(ctx, b.ID); err != nil {
		// ErrNoSuchLogging or any other meta error → no row.
		return
	}
	end := now()
	q := r.URL.Query()
	entry := &meta.AccessLogEntry{
		BucketID:    b.ID,
		Bucket:      bucket,
		Time:        start,
		RequestID:   r.Header.Get("X-Request-Id"),
		Principal:   principalFromContext(r),
		SourceIP:    clientSourceIP(r),
		Op:          deriveAccessLogOp(r.Method, key, q),
		Key:         key,
		Status:      rw.status,
		BytesSent:   rw.bytes,
		ObjectSize:  deriveObjectSize(r, rw),
		TotalTimeMS: int(end.Sub(start) / time.Millisecond),
		Referrer:    r.Header.Get("Referer"),
		UserAgent:   r.Header.Get("User-Agent"),
		VersionID:   q.Get("versionId"),
	}
	_ = m.Meta.EnqueueAccessLog(ctx, entry)
}

type accessLogWriter struct {
	http.ResponseWriter
	status      int
	bytes       int64
	wroteHeader bool
}

func (w *accessLogWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.status = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *accessLogWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.wroteHeader = true
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	return n, err
}

// accessLogSubresources lists query keys that, when present, become the
// resource segment of the AWS-style operation code (REST.<METHOD>.<RES>).
var accessLogSubresources = []string{
	"acl", "policy", "lifecycle", "cors", "tagging", "logging", "website",
	"versioning", "versions", "encryption", "object-lock", "publicAccessBlock",
	"ownershipControls", "notification", "replication", "uploads", "uploadId",
	"location", "requestPayment", "accelerate", "intelligent-tiering",
	"inventory", "metrics", "analytics", "select", "torrent", "restore",
	"retention", "legal-hold", "attributes", "partNumber",
}

func deriveAccessLogOp(method, key string, q url.Values) string {
	for _, sub := range accessLogSubresources {
		if q.Has(sub) {
			res := strings.ToUpper(strings.NewReplacer("-", "_").Replace(sub))
			return "REST." + method + "." + res
		}
	}
	if key != "" {
		return "REST." + method + ".OBJECT"
	}
	return "REST." + method + ".BUCKET"
}

func deriveObjectSize(r *http.Request, rw *accessLogWriter) int64 {
	switch r.Method {
	case http.MethodPut, http.MethodPost:
		if r.ContentLength > 0 {
			return r.ContentLength
		}
		return 0
	case http.MethodGet, http.MethodHead:
		if cl := rw.Header().Get("Content-Length"); cl != "" {
			if n, err := parseInt64(cl); err == nil {
				return n
			}
		}
		return rw.bytes
	}
	return 0
}

func parseInt64(s string) (int64, error) {
	var n int64
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, errInvalidContentLength
		}
		n = n*10 + int64(c-'0')
	}
	return n, nil
}

var errInvalidContentLength = errors.New("invalid content-length")
