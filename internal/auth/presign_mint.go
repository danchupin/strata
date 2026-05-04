package auth

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// PresignOptions feeds GeneratePresignedURL. Method/Host/Path/Region/AccessKey/
// Secret are required. Query carries any pre-existing query parameters that
// must be part of the signature (e.g. partNumber + uploadId on UploadPart).
// Expires <= 0 falls back to 5 minutes; capped to AWS's 7-day maximum.
type PresignOptions struct {
	Method    string
	Scheme    string
	Host      string
	Path      string
	Query     url.Values
	Region    string
	AccessKey string
	Secret    string
	Expires   time.Duration
	Now       time.Time
}

const (
	presignMaxExpires     = 7 * 24 * time.Hour
	presignDefaultExpires = 5 * time.Minute
)

var (
	errPresignMissingField = errors.New("presign: missing required option")
)

// GeneratePresignedURL mints a SigV4 presigned URL signing only the host
// header (so the browser can issue the request unmodified via fetch/XHR).
// The body is signed as UNSIGNED-PAYLOAD so callers can stream arbitrary
// bytes without recomputing the hash. Returns the absolute URL string.
//
// Verification flows through the existing parsePresigned + computeSignature
// path on the gateway side — the canonical-request shape used here matches
// canonicalRequestWithQuery, so a presigned URL minted by this function
// passes the gateway's own SigV4 check.
func GeneratePresignedURL(opts PresignOptions) (string, error) {
	if opts.Method == "" || opts.Host == "" || opts.Path == "" ||
		opts.Region == "" || opts.AccessKey == "" || opts.Secret == "" {
		return "", errPresignMissingField
	}
	scheme := opts.Scheme
	if scheme == "" {
		scheme = "https"
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	expires := opts.Expires
	if expires <= 0 {
		expires = presignDefaultExpires
	}
	if expires > presignMaxExpires {
		expires = presignMaxExpires
	}

	amzDate := now.Format(sigTimeFormat)
	day := now.Format(sigDateFormat)
	scope := credentialScope(day, opts.Region, sigServiceS3)
	signedHeaders := []string{"host"}

	q := url.Values{}
	if opts.Query != nil {
		for k, vs := range opts.Query {
			for _, v := range vs {
				q.Add(k, v)
			}
		}
	}
	q.Set("X-Amz-Algorithm", sigAlgorithm)
	q.Set("X-Amz-Credential", opts.AccessKey+"/"+scope)
	q.Set("X-Amz-Date", amzDate)
	q.Set("X-Amz-Expires", strconv.FormatInt(int64(expires.Seconds()), 10))
	q.Set("X-Amz-SignedHeaders", strings.Join(signedHeaders, ";"))

	canonicalQuery := canonicalQueryValues(q)
	req := &http.Request{
		Method: opts.Method,
		URL: &url.URL{
			Path: opts.Path,
		},
		Host:   opts.Host,
		Header: http.Header{},
	}
	canonical := canonicalRequestWithQuery(req, canonicalQuery, signedHeaders, unsignedBody)
	sts := stringToSign(sigAlgorithm, amzDate, scope, sha256Hex([]byte(canonical)))
	key := deriveSigningKey(opts.Secret, day, opts.Region, sigServiceS3)
	sig := hmacSHA256(key, []byte(sts))
	q.Set(presignSignatureParam, hexEncode(sig))

	u := url.URL{
		Scheme: scheme,
		Host:   opts.Host,
		Path:   opts.Path,
	}
	return u.String() + "?" + canonicalQueryValues(q), nil
}

// hexEncode is a small wrapper around encoding/hex so we don't drag the
// import into this file when the rest of the package already uses
// encoding/hex via sha256Hex; kept here so the callsite reads cleanly.
func hexEncode(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, v := range b {
		out[i*2] = digits[v>>4]
		out[i*2+1] = digits[v&0x0f]
	}
	return string(out)
}
