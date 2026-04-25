package s3api

import (
	"net/http"
	"strings"
	"time"
)

var ErrPreconditionFailed = APIError{
	Code:    "PreconditionFailed",
	Message: "At least one of the preconditions you specified did not hold",
	Status:  http.StatusPreconditionFailed,
}

// etagMatches honors quoted-string, bare-token, and * semantics. Both sides
// are compared after stripping surrounding quotes. A literal "*" always
// matches.
func etagMatches(header, etag string) bool {
	header = strings.TrimSpace(header)
	if header == "*" {
		return true
	}
	candidate := strings.TrimSpace(etag)
	candidate = strings.Trim(candidate, `"`)
	for _, part := range strings.Split(header, ",") {
		p := strings.TrimSpace(part)
		p = strings.Trim(p, `"`)
		if p == candidate {
			return true
		}
	}
	return false
}

// checkConditional evaluates If-Match, If-None-Match, If-Modified-Since,
// If-Unmodified-Since per RFC 7232 and returns (httpStatus, continue).
//   continue=false means the caller should short-circuit with the returned
//   status (304 or 412). continue=true means serve the full 200/206.
func checkConditional(h http.Header, etag string, mtime time.Time) (int, bool) {
	if v := h.Get("If-Match"); v != "" {
		if !etagMatches(v, etag) {
			return http.StatusPreconditionFailed, false
		}
	}
	if v := h.Get("If-Unmodified-Since"); v != "" {
		if t, err := http.ParseTime(v); err == nil && mtime.After(t.Add(time.Second)) {
			return http.StatusPreconditionFailed, false
		}
	}
	if v := h.Get("If-None-Match"); v != "" {
		if etagMatches(v, etag) {
			return http.StatusNotModified, false
		}
	} else if v := h.Get("If-Modified-Since"); v != "" {
		if t, err := http.ParseTime(v); err == nil && !mtime.After(t) {
			return http.StatusNotModified, false
		}
	}
	return 0, true
}
