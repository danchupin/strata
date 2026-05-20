package auth

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

type Middleware struct {
	Store CredentialsStore
	Mode  Mode
}

type DenyHandler func(w http.ResponseWriter, r *http.Request, err error)

func (m *Middleware) Wrap(next http.Handler, deny DenyHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if m.Mode == ModeOff {
			ctx := WithAuth(r.Context(), AnonymousIdentity())
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		if m.Mode == ModeOptional && r.Header.Get("Authorization") == "" && !hasPresignedParams(r) {
			ctx := WithAuth(r.Context(), AnonymousIdentity())
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}

		info, err := m.validate(r)
		if err != nil {
			deny(w, r, err)
			return
		}
		ctx := WithAuth(r.Context(), info)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (m *Middleware) validate(r *http.Request) (*AuthInfo, error) {
	if r.Header.Get("Authorization") == "" && hasPresignedParams(r) {
		return m.validatePresigned(r)
	}
	return m.validateHeader(r)
}

func (m *Middleware) validateHeader(r *http.Request) (*AuthInfo, error) {
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		return nil, ErrMissingSignature
	}
	parsed, err := parseAuthHeader(authHeader)
	if err != nil {
		return nil, err
	}
	reqDate := r.Header.Get("X-Amz-Date")
	if reqDate == "" {
		return nil, ErrMalformedAuth
	}
	t, err := time.Parse(sigTimeFormat, reqDate)
	if err != nil {
		return nil, ErrMalformedAuth
	}
	if skew := time.Since(t); skew < -sigMaxSkew || skew > sigMaxSkew {
		return nil, ErrClockSkew
	}
	cred, err := m.Store.Lookup(r.Context(), parsed.AccessKey)
	if err != nil {
		return nil, err
	}

	bodyHash := r.Header.Get("X-Amz-Content-Sha256")
	if bodyHash == "" {
		bodyHash = unsignedBody
	}
	canonical := canonicalRequest(r, parsed.SignedHeaders, bodyHash)
	expected := computeSignature(cred.Secret, parsed, reqDate, canonical)
	if !constantTimeEqual(expected, parsed.Signature) {
		return nil, ErrSignatureInvalid
	}
	if cred.SessionToken != "" {
		if r.Header.Get("X-Amz-Security-Token") != cred.SessionToken {
			return nil, ErrInvalidToken
		}
	}

	if bodyHash == streamingBody || bodyHash == streamingBodyTrailer {
		signingKey := deriveSigningKey(cred.Secret, parsed.Date, parsed.Region, parsed.Service)
		scope := credentialScope(parsed.Date, parsed.Region, parsed.Service)
		if bodyHash == streamingBodyTrailer {
			// US-009 sha256-only scope. X-Amz-Trailer carries the algo
			// name as a comma-separated list; only an exact
			// `x-amz-checksum-sha256` (case-insensitive, single value) is
			// accepted in Cycle 1. crc32 / crc32c / sha1 are parked
			// behind a P3 ROADMAP follow-up.
			algo := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Amz-Trailer")))
			if algo != trailerHeaderChecksumSh {
				return nil, ErrUnsupportedChecksumAlgorithm
			}
			r.Body = newStreamingTrailerReader(r.Body, signingKey, reqDate, scope, parsed.Signature)
		} else {
			r.Body = newStreamingReader(r.Body, signingKey, reqDate, scope, parsed.Signature)
		}
		if dec := r.Header.Get("X-Amz-Decoded-Content-Length"); dec != "" {
			if n, err := strconv.ParseInt(dec, 10, 64); err == nil {
				r.ContentLength = n
				r.Header.Set("Content-Length", dec)
			}
		}
		r.Header.Del("X-Amz-Decoded-Content-Length")
	}

	return &AuthInfo{
		AccessKey: cred.AccessKey,
		Owner:     cred.Owner,
	}, nil
}

func (m *Middleware) validatePresigned(r *http.Request) (*AuthInfo, error) {
	parsed, reqTime, expires, err := parsePresigned(r)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	if now.Before(reqTime.Add(-sigMaxSkew)) || now.After(reqTime.Add(time.Duration(expires)*time.Second)) {
		return nil, ErrClockSkew
	}
	cred, err := m.Store.Lookup(r.Context(), parsed.AccessKey)
	if err != nil {
		return nil, err
	}
	query := canonicalQueryWithout(r.URL, presignSignatureParam)
	canonical := canonicalRequestWithQuery(r, query, parsed.SignedHeaders, unsignedBody)
	expected := computeSignature(cred.Secret, parsed, reqTime.UTC().Format(sigTimeFormat), canonical)
	if !constantTimeEqual(expected, parsed.Signature) {
		return nil, ErrSignatureInvalid
	}
	if cred.SessionToken != "" {
		token := r.Header.Get("X-Amz-Security-Token")
		if token == "" {
			token = r.URL.Query().Get("X-Amz-Security-Token")
		}
		if token != cred.SessionToken {
			return nil, ErrInvalidToken
		}
	}
	return &AuthInfo{
		AccessKey: cred.AccessKey,
		Owner:     cred.Owner,
	}, nil
}

func constantTimeEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := 0; i < len(a); i++ {
		v |= a[i] ^ b[i]
	}
	return v == 0
}
