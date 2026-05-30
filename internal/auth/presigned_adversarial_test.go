package auth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// US-004 — presigned-URL adversarial auth matrix.
//
// Presigned URLs are the gateway's second auth boundary (no Authorization
// header — the whole credential lives in the query string). validatePresigned
// (middleware.go:143) re-derives the signature from the canonical query (minus
// X-Amz-Signature) over an UNSIGNED-PAYLOAD body and enforces the
// X-Amz-Expires window. This proves each tampering / lifetime class is
// rejected with the exact sentinel, and documents the one deliberate
// non-rejection: replay within the expiry window IS permitted (AWS parity — no
// server-side nonce).
//
// Reuses the package-level adv* credentials, stubStore, newAdvMiddleware, and
// wantS3Code from sigv4_adversarial_test.go (same package) so the
// sentinel→S3-code contract stays pinned in one place.

// presignOpts parameterises the valid presigned request built by signPresigned.
// Zero values yield a correctly-signed GET that mw.validate accepts.
type presignOpts struct {
	method  string    // default GET
	rawURL  string    // default https://<host>/bkt/key (may carry extra signed query)
	region  string    // scope region; default advRegion
	service string    // scope service; default advService
	when    time.Time // X-Amz-Date; default time.Now().UTC()
	expires int64     // X-Amz-Expires seconds; default 900
	host    string    // Host header; default advHost
}

// signPresigned builds a fully-valid SigV4 presigned request from opts. It
// mirrors validatePresigned exactly: canonicalQueryWithout(X-Amz-Signature) →
// canonicalRequestWithQuery over UNSIGNED-PAYLOAD → computeSignature with the
// full timestamp as reqDate. Any later tamper is the only divergence.
func signPresigned(t *testing.T, o presignOpts) *http.Request {
	t.Helper()
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
	if o.when.IsZero() {
		o.when = time.Now().UTC()
	}
	if o.expires == 0 {
		o.expires = 900
	}
	if o.host == "" {
		o.host = advHost
	}

	req := httptest.NewRequest(o.method, o.rawURL, nil)
	req.Host = o.host
	ts := o.when.Format(sigTimeFormat)
	day := ts[:8]

	q := req.URL.Query()
	q.Set("X-Amz-Algorithm", sigAlgorithm)
	q.Set("X-Amz-Credential", advAccessKey+"/"+day+"/"+o.region+"/"+o.service+"/"+sigTerminator)
	q.Set("X-Amz-Date", ts)
	q.Set("X-Amz-Expires", strconv.FormatInt(o.expires, 10))
	q.Set("X-Amz-SignedHeaders", "host")
	req.URL.RawQuery = q.Encode()

	parsed := &parsedAuth{
		AccessKey:     advAccessKey,
		Date:          day,
		Region:        o.region,
		Service:       o.service,
		SignedHeaders: []string{"host"},
	}
	query := canonicalQueryWithout(req.URL, presignSignatureParam)
	canonical := canonicalRequestWithQuery(req, query, parsed.SignedHeaders, unsignedBody)
	sig := computeSignature(advSecret, parsed, ts, canonical)

	q.Set(presignSignatureParam, sig)
	req.URL.RawQuery = q.Encode()
	return req
}

func TestPresigned_AdversarialMatrix(t *testing.T) {
	cases := []struct {
		name     string
		build    func(t *testing.T) *http.Request
		wantErr  error
		wantCode string
	}{
		{
			name: "expired (X-Amz-Expires elapsed before now)",
			build: func(t *testing.T) *http.Request {
				// Signed 30m ago with a 60s lifetime → now > reqTime+expires.
				return signPresigned(t, presignOpts{when: time.Now().Add(-30 * time.Minute).UTC(), expires: 60})
			},
			wantErr:  ErrClockSkew,
			wantCode: "RequestTimeTooSkewed",
		},
		{
			name: "future-dated (X-Amz-Date beyond +skew)",
			build: func(t *testing.T) *http.Request {
				return signPresigned(t, presignOpts{when: time.Now().Add(30 * time.Minute).UTC()})
			},
			wantErr:  ErrClockSkew,
			wantCode: "RequestTimeTooSkewed",
		},
		{
			name: "tampered signed query param (value mutated after signing)",
			build: func(t *testing.T) *http.Request {
				r := signPresigned(t, presignOpts{rawURL: "https://" + advHost + "/bkt/key?response-content-type=text%2Fplain"})
				q := r.URL.Query() // keeps X-Amz-Signature; only the signed param changes
				q.Set("response-content-type", "application/json")
				r.URL.RawQuery = q.Encode()
				return r
			},
			wantErr:  ErrSignatureInvalid,
			wantCode: "SignatureDoesNotMatch",
		},
		{
			name: "method mismatch (signed GET replayed as PUT)",
			build: func(t *testing.T) *http.Request {
				r := signPresigned(t, presignOpts{method: http.MethodGet})
				r.Method = http.MethodPut // canonical request pins the verb
				return r
			},
			wantErr:  ErrSignatureInvalid,
			wantCode: "SignatureDoesNotMatch",
		},
		{
			name: "tampered path (URL mutated after signing)",
			build: func(t *testing.T) *http.Request {
				r := signPresigned(t, presignOpts{})
				r.URL.Path = "/bkt/other-key"
				return r
			},
			wantErr:  ErrSignatureInvalid,
			wantCode: "SignatureDoesNotMatch",
		},
		{
			name: "tampered SignedHeaders scope (host dropped after signing)",
			build: func(t *testing.T) *http.Request {
				r := signPresigned(t, presignOpts{})
				q := r.URL.Query()
				q.Set("X-Amz-SignedHeaders", "host;x-amz-date")
				r.URL.RawQuery = q.Encode()
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
			if _, err := mw.validate(signPresigned(t, presignOpts{})); err != nil {
				t.Fatalf("positive control rejected a valid presigned request: %v", err)
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

// TestPresigned_ValidVariantsPass is the positive-control suite: every signing
// variant the matrix tampers from must itself be accepted.
func TestPresigned_ValidVariantsPass(t *testing.T) {
	mw := newAdvMiddleware()
	variants := []struct {
		name string
		opts presignOpts
	}{
		{"default GET", presignOpts{}},
		{"PUT method", presignOpts{method: http.MethodPut}},
		{"extra signed query param", presignOpts{rawURL: "https://" + advHost + "/bkt/key?response-content-type=text%2Fplain"}},
		{"subpath key", presignOpts{rawURL: "https://" + advHost + "/bkt/a/b/c.txt"}},
		{"non-default region self-consistent", presignOpts{region: "ap-south-1"}},
		{"max 7-day expiry", presignOpts{expires: 7 * 24 * 3600}},
		{"recently signed (1m ago, within window)", presignOpts{when: time.Now().Add(-1 * time.Minute).UTC()}},
	}
	for _, v := range variants {
		t.Run(v.name, func(t *testing.T) {
			info, err := mw.validate(signPresigned(t, v.opts))
			if err != nil {
				t.Fatalf("valid presigned request rejected: %v", err)
			}
			if info == nil || info.AccessKey != advAccessKey {
				t.Fatalf("unexpected identity: %+v", info)
			}
		})
	}
}

// TestPresigned_ValidReplayWithinWindow documents the one deliberate
// non-rejection: a presigned URL re-validated inside its expiry window passes
// every time. Strata holds no server-side nonce, matching AWS S3 — replay
// within the window is permitted by design, NOT a finding.
func TestPresigned_ValidReplayWithinWindow(t *testing.T) {
	mw := newAdvMiddleware()
	req := signPresigned(t, presignOpts{expires: 900})
	for i := 0; i < 3; i++ {
		if _, err := mw.validate(req); err != nil {
			t.Fatalf("replay attempt %d rejected (replay within window must pass): %v", i+1, err)
		}
	}
}
