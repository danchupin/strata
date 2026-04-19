package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	sigAlgorithm   = "AWS4-HMAC-SHA256"
	sigTerminator  = "aws4_request"
	sigServiceS3   = "s3"
	sigTimeFormat  = "20060102T150405Z"
	sigDateFormat  = "20060102"
	sigMaxSkew     = 15 * time.Minute
	streamingBody  = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD"
	unsignedBody   = "UNSIGNED-PAYLOAD"
	emptyBodyHash  = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

type parsedAuth struct {
	AccessKey     string
	Date          string
	Region        string
	Service       string
	SignedHeaders []string
	Signature     string
}

func parseAuthHeader(h string) (*parsedAuth, error) {
	if !strings.HasPrefix(h, sigAlgorithm+" ") {
		return nil, ErrMalformedAuth
	}
	p := &parsedAuth{}
	rest := strings.TrimPrefix(h, sigAlgorithm+" ")
	for _, pair := range strings.Split(rest, ",") {
		pair = strings.TrimSpace(pair)
		k, v, ok := strings.Cut(pair, "=")
		if !ok {
			return nil, ErrMalformedAuth
		}
		switch k {
		case "Credential":
			credParts := strings.Split(v, "/")
			if len(credParts) != 5 || credParts[4] != sigTerminator {
				return nil, ErrMalformedAuth
			}
			p.AccessKey = credParts[0]
			p.Date = credParts[1]
			p.Region = credParts[2]
			p.Service = credParts[3]
		case "SignedHeaders":
			p.SignedHeaders = strings.Split(v, ";")
		case "Signature":
			p.Signature = v
		}
	}
	if p.AccessKey == "" || p.Signature == "" || len(p.SignedHeaders) == 0 {
		return nil, ErrMalformedAuth
	}
	return p, nil
}

func deriveSigningKey(secret, date, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), []byte(date))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	return hmacSHA256(kService, []byte(sigTerminator))
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func canonicalRequest(r *http.Request, signedHeaders []string, hashedPayload string) string {
	var b strings.Builder
	b.WriteString(r.Method)
	b.WriteByte('\n')
	b.WriteString(canonicalURI(r.URL.EscapedPath()))
	b.WriteByte('\n')
	b.WriteString(canonicalQuery(r.URL))
	b.WriteByte('\n')
	b.WriteString(canonicalHeaders(r, signedHeaders))
	b.WriteByte('\n')
	b.WriteString(strings.Join(signedHeaders, ";"))
	b.WriteByte('\n')
	b.WriteString(hashedPayload)
	return b.String()
}

func canonicalURI(path string) string {
	if path == "" {
		return "/"
	}
	decoded, err := url.PathUnescape(path)
	if err != nil {
		decoded = path
	}
	segments := strings.Split(decoded, "/")
	for i, seg := range segments {
		segments[i] = uriEncode(seg, false)
	}
	return strings.Join(segments, "/")
}

func canonicalQuery(u *url.URL) string {
	vals := u.Query()
	keys := make([]string, 0, len(vals))
	for k := range vals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	first := true
	for _, k := range keys {
		vs := append([]string(nil), vals[k]...)
		sort.Strings(vs)
		for _, v := range vs {
			if !first {
				b.WriteByte('&')
			}
			first = false
			b.WriteString(uriEncode(k, true))
			b.WriteByte('=')
			b.WriteString(uriEncode(v, true))
		}
	}
	return b.String()
}

func uriEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9'):
			b.WriteByte(c)
		case c == '-' || c == '_' || c == '.' || c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func canonicalHeaders(r *http.Request, signedHeaders []string) string {
	var b strings.Builder
	for _, h := range signedHeaders {
		b.WriteString(h)
		b.WriteByte(':')
		b.WriteString(trimHeaderValue(headerValue(r, h)))
		b.WriteByte('\n')
	}
	return b.String()
}

func headerValue(r *http.Request, name string) string {
	if name == "host" {
		return r.Host
	}
	vals := r.Header.Values(http.CanonicalHeaderKey(name))
	if len(vals) == 0 {
		return ""
	}
	return strings.Join(vals, ",")
}

func trimHeaderValue(v string) string {
	v = strings.TrimSpace(v)
	return strings.Join(strings.Fields(v), " ")
}

func stringToSign(algo, reqDate, scope, hashedCanonical string) string {
	return algo + "\n" + reqDate + "\n" + scope + "\n" + hashedCanonical
}

func credentialScope(date, region, service string) string {
	return date + "/" + region + "/" + service + "/" + sigTerminator
}

func computeSignature(secret string, parsed *parsedAuth, reqDate string, canonical string) string {
	scope := credentialScope(parsed.Date, parsed.Region, parsed.Service)
	sts := stringToSign(sigAlgorithm, reqDate, scope, sha256Hex([]byte(canonical)))
	key := deriveSigningKey(secret, parsed.Date, parsed.Region, parsed.Service)
	return hex.EncodeToString(hmacSHA256(key, []byte(sts)))
}
