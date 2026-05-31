package auth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// US-003 — SigV4 adversarial auth matrix.
//
// Auth is the gateway's security boundary; this proves SigV4 rejects every
// tampering class with the exact sentinel. Each case starts from a fully-valid
// signed request (the positive control) and applies one tamper, so a rejection
// is provably the tamper and not a broken setup. The sentinel→S3-code mapping
// asserted here is the contract enforced by s3api.WriteAuthDenied
// (internal/s3api/errors.go:180): "signature does not match" →
// SignatureDoesNotMatch, "request time outside permitted window" →
// RequestTimeTooSkewed. We assert the mapping in-package (no s3api import; that
// would be an import cycle) to keep the contract pinned at the auth boundary.

const (
	advAccessKey = "AKIAADVERSARIAL0001"
	advSecret    = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
	advRegion    = "us-east-1"
	advService   = "s3"
	advHost      = "strata.example.com"
)

// realPayloadHash is the SHA-256 of the empty body — a concrete signed-payload
// hash, distinct from the UNSIGNED-PAYLOAD sentinel, so the unsigned-vs-signed
// mismatch case has two genuinely different values to swap between.
const realPayloadHash = emptyBodyHash

// signOpts parameterises the valid request built by signV4. Defaults (zero
// values) yield a correctly-signed GET that mw.validate accepts.
type signOpts struct {
	method   string    // default GET
	rawURL   string    // default https://<host>/bkt/key
	region   string    // scope region; default advRegion
	service  string    // scope service; default advService
	bodyHash string    // X-Amz-Content-Sha256 value; default realPayloadHash
	when     time.Time // request time; default time.Now().UTC()
	host     string    // Host header; default advHost
}

func (o signOpts) withDefaults() signOpts {
	if o.method == "" {
		o.method = http.MethodGet
	}
	if o.rawURL == "" {
		o.rawURL = "https://" + advHost + "/bkt/key"
	}
	if o.region == "" {
		o.region = advRegion
	}
	if o.service == "" {
		o.service = advService
	}
	if o.bodyHash == "" {
		o.bodyHash = realPayloadHash
	}
	if o.when.IsZero() {
		o.when = time.Now().UTC()
	}
	if o.host == "" {
		o.host = advHost
	}
	return o
}

// signV4 builds a fully-valid SigV4-signed request from opts. It reuses the
// production canonicalRequest/computeSignature so the signature matches exactly
// what mw.validate recomputes — any later tamper is the only divergence.
func signV4(t *testing.T, o signOpts) *http.Request {
	t.Helper()
	o = o.withDefaults()

	req := httptest.NewRequest(o.method, o.rawURL, nil)
	req.Host = o.host
	reqDate := o.when.Format(sigTimeFormat)
	day := reqDate[:8]
	req.Header.Set("X-Amz-Date", reqDate)
	req.Header.Set("X-Amz-Content-Sha256", o.bodyHash)

	signedHeaders := []string{"host", "x-amz-content-sha256", "x-amz-date"}
	parsed := &parsedAuth{
		AccessKey:     advAccessKey,
		Date:          day,
		Region:        o.region,
		Service:       o.service,
		SignedHeaders: signedHeaders,
	}
	canonical := canonicalRequest(req, signedHeaders, o.bodyHash)
	sig := computeSignature(advSecret, parsed, reqDate, canonical)

	req.Header.Set("Authorization", buildAuthHeader(parsed, sig))
	return req
}

func buildAuthHeader(p *parsedAuth, sig string) string {
	return sigAlgorithm +
		" Credential=" + p.AccessKey + "/" + p.Date + "/" + p.Region + "/" + p.Service + "/" + sigTerminator +
		", SignedHeaders=" + strings.Join(p.SignedHeaders, ";") +
		", Signature=" + sig
}

// rewriteScope rebuilds the Authorization header with a mutated credential
// scope (region/service) while leaving the original signature untouched — the
// server re-derives the signing key from the tampered scope and mismatches.
func rewriteScope(t *testing.T, req *http.Request, region, service string) {
	t.Helper()
	parsed, err := parseAuthHeader(req.Header.Get("Authorization"))
	if err != nil {
		t.Fatalf("re-parse auth header: %v", err)
	}
	if region != "" {
		parsed.Region = region
	}
	if service != "" {
		parsed.Service = service
	}
	req.Header.Set("Authorization", buildAuthHeader(parsed, parsed.Signature))
}

// replaceSignature swaps the Signature= component for newSig, leaving the rest
// of the canonical material intact.
func replaceSignature(t *testing.T, req *http.Request, newSig string) {
	t.Helper()
	parsed, err := parseAuthHeader(req.Header.Get("Authorization"))
	if err != nil {
		t.Fatalf("re-parse auth header: %v", err)
	}
	req.Header.Set("Authorization", buildAuthHeader(parsed, newSig))
}

func newAdvMiddleware() *Middleware {
	return &Middleware{
		Store: &stubStore{cred: &Credential{
			AccessKey: advAccessKey,
			Secret:    advSecret,
			Owner:     "adversary-test",
		}},
		Mode: ModeRequired,
	}
}

// wantS3Code mirrors s3api.WriteAuthDenied's sentinel→code switch so the
// adversarial matrix pins the operator-visible S3 error code, not just the
// internal sentinel. Kept in lockstep with internal/s3api/errors.go:180.
func wantS3Code(err error) string {
	switch {
	case errors.Is(err, ErrSignatureInvalid):
		return "SignatureDoesNotMatch"
	case errors.Is(err, ErrClockSkew):
		return "RequestTimeTooSkewed"
	case errors.Is(err, ErrMalformedAuth):
		return "AccessDenied" // default arm of WriteAuthDenied
	case errors.Is(err, ErrMissingSignature):
		return "MissingSecurityHeader"
	default:
		return ""
	}
}

func TestSigV4_AdversarialMatrix(t *testing.T) {
	cases := []struct {
		name     string
		build    func(t *testing.T) *http.Request
		wantErr  error
		wantCode string
	}{
		{
			name:     "bad signature (garbage hex, same length)",
			build:    func(t *testing.T) *http.Request { r := signV4(t, signOpts{}); replaceSignature(t, r, strings.Repeat("a", 64)); return r },
			wantErr:  ErrSignatureInvalid,
			wantCode: "SignatureDoesNotMatch",
		},
		{
			name:     "expired request (past skew beyond 15m)",
			build:    func(t *testing.T) *http.Request { return signV4(t, signOpts{when: time.Now().Add(-20 * time.Minute).UTC()}) },
			wantErr:  ErrClockSkew,
			wantCode: "RequestTimeTooSkewed",
		},
		{
			name:     "future-dated request (future skew beyond 15m)",
			build:    func(t *testing.T) *http.Request { return signV4(t, signOpts{when: time.Now().Add(20 * time.Minute).UTC()}) },
			wantErr:  ErrClockSkew,
			wantCode: "RequestTimeTooSkewed",
		},
		{
			name: "wrong region in scope (signature for us-east-1, scope claims eu-west-1)",
			build: func(t *testing.T) *http.Request {
				r := signV4(t, signOpts{})
				rewriteScope(t, r, "eu-west-1", "")
				return r
			},
			wantErr:  ErrSignatureInvalid,
			wantCode: "SignatureDoesNotMatch",
		},
		{
			name: "wrong service in scope (s3 signature, scope claims iam)",
			build: func(t *testing.T) *http.Request {
				r := signV4(t, signOpts{})
				rewriteScope(t, r, "", "iam")
				return r
			},
			wantErr:  ErrSignatureInvalid,
			wantCode: "SignatureDoesNotMatch",
		},
		{
			name: "tampered path (URL mutated after signing)",
			build: func(t *testing.T) *http.Request {
				r := signV4(t, signOpts{})
				r.URL.Path = "/bkt/other-key"
				return r
			},
			wantErr:  ErrSignatureInvalid,
			wantCode: "SignatureDoesNotMatch",
		},
		{
			name: "tampered query (param added after signing)",
			build: func(t *testing.T) *http.Request {
				r := signV4(t, signOpts{})
				r.URL.RawQuery = "x-injected=1"
				return r
			},
			wantErr:  ErrSignatureInvalid,
			wantCode: "SignatureDoesNotMatch",
		},
		{
			name: "tampered signed header (Host mutated after signing)",
			build: func(t *testing.T) *http.Request {
				r := signV4(t, signOpts{})
				r.Host = "attacker.example.com"
				return r
			},
			wantErr:  ErrSignatureInvalid,
			wantCode: "SignatureDoesNotMatch",
		},
		{
			name: "missing Host (empty after signing)",
			build: func(t *testing.T) *http.Request {
				r := signV4(t, signOpts{})
				r.Host = ""
				return r
			},
			wantErr:  ErrSignatureInvalid,
			wantCode: "SignatureDoesNotMatch",
		},
		{
			name: "missing x-amz-content-sha256 (signed header deleted after signing)",
			build: func(t *testing.T) *http.Request {
				r := signV4(t, signOpts{})
				r.Header.Del("X-Amz-Content-Sha256")
				return r
			},
			wantErr:  ErrSignatureInvalid,
			wantCode: "SignatureDoesNotMatch",
		},
		{
			name: "missing x-amz-date (mandatory header absent)",
			build: func(t *testing.T) *http.Request {
				r := signV4(t, signOpts{})
				r.Header.Del("X-Amz-Date")
				return r
			},
			wantErr:  ErrMalformedAuth,
			wantCode: "AccessDenied",
		},
		{
			name: "unsigned-payload vs signed-payload mismatch (signed UNSIGNED-PAYLOAD, sent real hash)",
			build: func(t *testing.T) *http.Request {
				r := signV4(t, signOpts{bodyHash: unsignedBody})
				r.Header.Set("X-Amz-Content-Sha256", realPayloadHash)
				return r
			},
			wantErr:  ErrSignatureInvalid,
			wantCode: "SignatureDoesNotMatch",
		},
		{
			name: "signed-payload vs unsigned-payload mismatch (signed real hash, sent UNSIGNED-PAYLOAD)",
			build: func(t *testing.T) *http.Request {
				r := signV4(t, signOpts{bodyHash: realPayloadHash})
				r.Header.Set("X-Amz-Content-Sha256", unsignedBody)
				return r
			},
			wantErr:  ErrSignatureInvalid,
			wantCode: "SignatureDoesNotMatch",
		},
	}

	mw := newAdvMiddleware()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Positive control: the un-tampered baseline must pass, so the
			// rejection below is provably the tamper, not a broken setup.
			if _, err := mw.validate(signV4(t, signOpts{})); err != nil {
				t.Fatalf("positive control rejected a valid request: %v", err)
			}

			_, err := mw.validate(tc.build(t))
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("got err %v, want sentinel %v", err, tc.wantErr)
			}
			if got := wantS3Code(err); got != tc.wantCode {
				t.Fatalf("S3 code for %v: got %q want %q", err, got, tc.wantCode)
			}
		})
	}
}

// TestSigV4_ValidVariantsPass is the explicit positive-control suite: each
// signing variant the matrix tampers from must itself be accepted, proving the
// negatives isolate the tamper rather than a structural rejection.
func TestSigV4_ValidVariantsPass(t *testing.T) {
	mw := newAdvMiddleware()
	variants := []struct {
		name string
		opts signOpts
	}{
		{"empty-body signed hash", signOpts{bodyHash: realPayloadHash}},
		{"unsigned payload", signOpts{bodyHash: unsignedBody}},
		{"subpath key", signOpts{rawURL: "https://" + advHost + "/bkt/a/b/c.txt"}},
		{"with query string", signOpts{rawURL: "https://" + advHost + "/bkt/key?versionId=null&partNumber=2"}},
		{"PUT method", signOpts{method: http.MethodPut}},
		{"non-default region self-consistent", signOpts{region: "ap-south-1"}},
		{"skew at the edge (14m past)", signOpts{when: time.Now().Add(-14 * time.Minute).UTC()}},
	}
	for _, v := range variants {
		t.Run(v.name, func(t *testing.T) {
			info, err := mw.validate(signV4(t, v.opts))
			if err != nil {
				t.Fatalf("valid request rejected: %v", err)
			}
			if info == nil || info.AccessKey != advAccessKey {
				t.Fatalf("unexpected identity: %+v", info)
			}
		})
	}
}
