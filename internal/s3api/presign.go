package s3api

import (
	"encoding/xml"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/meta"
)

// presignSignatureParam is the SigV4 query parameter that marks a request
// as presigned. Mirrored from internal/auth so the gateway can branch on
// presigned vs. header-signed requests without depending on the auth
// package's internals.
const presignSignatureParam = "X-Amz-Signature"

// defaultBackendPresignExpires is the lifetime applied when the original
// presigned request's X-Amz-Expires is missing or unparseable. Matches the
// SDK default so behaviour is stable across clients.
const defaultBackendPresignExpires = 15 * time.Minute

// putBucketBackendPresign toggles the per-bucket BackendPresign flag
// (US-016). Body is BackendPresignConfiguration XML; an empty body or
// <Enabled>false</Enabled> disables passthrough.
func (s *Server) putBucketBackendPresign(w http.ResponseWriter, r *http.Request, bucket string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil {
		writeError(w, r, ErrMalformedXML)
		return
	}
	enabled := false
	if len(body) > 0 {
		var doc backendPresignConfiguration
		if err := xml.Unmarshal(body, &doc); err != nil {
			writeError(w, r, ErrMalformedXML)
			return
		}
		enabled = doc.Enabled
	}
	if err := s.Meta.SetBucketBackendPresign(r.Context(), bucket, enabled); err != nil {
		mapMetaErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// getBucketBackendPresign returns the per-bucket BackendPresign flag.
func (s *Server) getBucketBackendPresign(w http.ResponseWriter, r *http.Request, bucket string) {
	b, err := s.Meta.GetBucket(r.Context(), bucket)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	writeXML(w, http.StatusOK, backendPresignConfiguration{Enabled: b.BackendPresign})
}

// maybeBackendPresignRedirect attempts to satisfy a GET-by-presigned-URL
// with a 307 redirect to a backend-credentialled URL (US-016 passthrough).
// Returns true when the redirect was issued (caller must not write further
// response data); false when the gateway should fall through to its
// in-process serve path.
//
// All four conditions must hold:
//   - request was authenticated via SigV4 query parameters (presigned)
//   - bucket has BackendPresign enabled
//   - data backend implements data.PresignBackend
//   - object's manifest carries a BackendRef (s3-shape, not chunks-shape)
//
// On any miss (incl. backend presign error) the function returns false and
// logs at WARN — the gateway continues to serve the bytes itself so the
// client always gets a response.
func (s *Server) maybeBackendPresignRedirect(w http.ResponseWriter, r *http.Request, b *meta.Bucket, o *meta.Object) bool {
	if !b.BackendPresign {
		return false
	}
	if r.URL.Query().Get(presignSignatureParam) == "" {
		return false
	}
	pb, ok := s.Data.(data.PresignBackend)
	if !ok {
		return false
	}
	if o.Manifest == nil || o.Manifest.BackendRef == nil {
		return false
	}

	expires := parsePresignExpires(r.URL.Query().Get("X-Amz-Expires"))
	urlStr, err := pb.PresignGetObject(r.Context(), o.Manifest, expires)
	if err != nil {
		slog.Warn("s3 backend presign passthrough: mint failed; serving inline",
			"bucket", b.Name, "key", o.Key, "err", err)
		return false
	}

	host := ""
	if u, err := url.Parse(urlStr); err == nil {
		host = u.Host
	}
	// Audit-log the passthrough so the redirect is captured even though
	// the data fetch will not hit Strata. structured fields keep the line
	// machine-parseable for downstream log aggregation.
	slog.Info("s3 backend presign passthrough",
		"bucket", b.Name,
		"key", o.Key,
		"version_id", o.VersionID,
		"backend_host", host,
		"expires_seconds", int(expires/time.Second),
		"access_key", auth.FromContext(r.Context()).AccessKey,
		"presign_passthrough", true,
	)

	// 307 preserves method + headers (including Range) on the redirected
	// request so partial-content semantics still work after passthrough.
	w.Header().Set("Location", urlStr)
	w.WriteHeader(http.StatusTemporaryRedirect)
	return true
}

// parsePresignExpires reads the original presigned request's X-Amz-Expires
// (seconds) and converts to time.Duration. Falls back to
// defaultBackendPresignExpires on missing or invalid input — never returns
// a non-positive duration so callers don't have to defend against it.
func parsePresignExpires(raw string) time.Duration {
	if raw == "" {
		return defaultBackendPresignExpires
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		return defaultBackendPresignExpires
	}
	return time.Duration(n) * time.Second
}
