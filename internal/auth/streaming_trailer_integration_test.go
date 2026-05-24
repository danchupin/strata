//go:build integration

package auth

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestStreamingTrailerRoundtripViaMiddleware drives hand-constructed
// STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER PUTs through the real auth
// Middleware against an httptest.Server for all 4 supported trailer
// checksum algos (sha256 / sha1 / crc32 / crc32c). Verifies (a) each request
// passes validateHeader, (b) the inner handler reads back the decoded
// plaintext, and (c) the trailer-signature + body-checksum gates are
// exercised on the hot path. Mirrors the smoke-signed shape but stays
// in-process (no dependency on a system-installed aws-cli 2.22+).
func TestStreamingTrailerRoundtripViaMiddleware(t *testing.T) {
	type algo struct {
		name   string
		header string
		newH   func() hash.Hash
	}
	cases := []algo{
		{"sha256", "x-amz-checksum-sha256", sha256.New},
		{"sha1", "x-amz-checksum-sha1", sha1.New},
		{"crc32", "x-amz-checksum-crc32", func() hash.Hash { return crc32.NewIEEE() }},
		{"crc32c", "x-amz-checksum-crc32c", func() hash.Hash { return crc32.New(crc32CastagnoliTable) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			runTrailerRoundtrip(t, c.header, c.newH)
		})
	}
}

func runTrailerRoundtrip(t *testing.T, trailerHeader string, newH func() hash.Hash) {
	t.Helper()
	const (
		accessKey = "AKIAIOSFODNN7EXAMPLE"
		secret    = streamSecret
		owner     = "tester"
		region    = "us-east-1"
		service   = "s3"
	)
	store := NewStaticStore(map[string]*Credential{
		accessKey: {AccessKey: accessKey, Secret: secret, Owner: owner},
	})
	mw := &Middleware{Store: store, Mode: ModeRequired}

	var gotBody []byte
	var gotOwner string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		gotBody = b
		if info := FromContext(r.Context()); info != nil {
			gotOwner = info.Owner
		}
		w.WriteHeader(http.StatusOK)
	})
	handler := mw.Wrap(inner, func(w http.ResponseWriter, r *http.Request, err error) {
		http.Error(w, err.Error(), http.StatusForbidden)
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()

	plain := bytes.Repeat([]byte("aws-chunked-trailer integration "), 256)
	host := strings.TrimPrefix(ts.URL, "http://")
	body, contentLen, headers := buildTrailerPUT(t, secret, accessKey, region, service, host, "/b/k", plain, trailerHeader, newH)

	req, err := http.NewRequest(http.MethodPut, ts.URL+"/b/k", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.ContentLength = int64(len(body))
	req.Header.Set("Content-Length", strconv.Itoa(len(body)))
	req.Header.Set("X-Amz-Decoded-Content-Length", strconv.Itoa(contentLen))
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT roundtrip: %v", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", resp.StatusCode, string(respBody))
	}
	if !bytes.Equal(gotBody, plain) {
		t.Fatalf("decoded body mismatch: got %d bytes, want %d", len(gotBody), len(plain))
	}
	if gotOwner != owner {
		t.Fatalf("decoded owner mismatch: got %q want %q", gotOwner, owner)
	}
}

// buildTrailerPUT constructs the full SigV4-signed
// STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER wire payload + the
// Authorization / X-Amz-* headers for the test request under the supplied
// trailer checksum algorithm.
func buildTrailerPUT(t *testing.T, secret, accessKey, region, service, host, urlPath string, plain []byte, trailerHeader string, newH func() hash.Hash) (body []byte, contentLen int, headers map[string]string) {
	t.Helper()
	now := time.Now().UTC()
	ts := now.Format(sigTimeFormat)
	date := now.Format(sigDateFormat)
	signingKey := deriveSigningKey(secret, date, region, service)
	scope := credentialScope(date, region, service)

	prev := ""
	var chunks [][]byte
	if len(plain) > 0 {
		chunks = [][]byte{plain}
	}

	signed := signedHeadersFor()
	req := &http.Request{
		Method: http.MethodPut,
		URL:    mustParse(urlPath),
		Host:   host,
		Header: http.Header{},
	}
	req.Header.Set("Host", host)
	req.Header.Set("X-Amz-Content-Sha256", streamingBodyTrailer)
	req.Header.Set("X-Amz-Date", ts)
	req.Header.Set("X-Amz-Trailer", trailerHeader)
	req.Header.Set("X-Amz-Decoded-Content-Length", strconv.Itoa(len(plain)))
	canonical := canonicalRequest(req, signed, streamingBodyTrailer)
	parsed := &parsedAuth{
		AccessKey: accessKey,
		Date:      date,
		Region:    region,
		Service:   service,
	}
	seedSig := computeSignature(secret, parsed, ts, canonical)
	prev = seedSig

	for _, c := range chunks {
		sig := computeChunkSignature(signingKey, ts, scope, prev, c)
		body = append(body, []byte(fmt.Sprintf("%x;chunk-signature=%s\r\n", len(c), sig))...)
		body = append(body, c...)
		body = append(body, []byte("\r\n")...)
		prev = sig
	}
	finalSig := computeChunkSignature(signingKey, ts, scope, prev, nil)
	body = append(body, []byte(fmt.Sprintf("0;chunk-signature=%s\r\n", finalSig))...)

	h := newH()
	h.Write(plain)
	b64 := base64.StdEncoding.EncodeToString(h.Sum(nil))
	canonicalTrailers := trailerHeader + ":" + b64 + "\n"
	trailerSig := computeTrailerSignature(signingKey, ts, scope, finalSig, canonicalTrailers)
	body = append(body, []byte(trailerHeader+":"+b64+"\r\n")...)
	body = append(body, []byte("x-amz-trailer-signature:"+trailerSig+"\r\n")...)
	body = append(body, []byte("\r\n")...)

	authz := fmt.Sprintf("%s Credential=%s/%s/%s/%s/%s, SignedHeaders=%s, Signature=%s",
		sigAlgorithm, accessKey, date, region, service, sigTerminator,
		strings.Join(signed, ";"), seedSig)

	return body, len(plain), map[string]string{
		"X-Amz-Content-Sha256":         streamingBodyTrailer,
		"X-Amz-Date":                   ts,
		"X-Amz-Trailer":                trailerHeader,
		"X-Amz-Decoded-Content-Length": strconv.Itoa(len(plain)),
		"Authorization":                authz,
	}
}

func signedHeadersFor() []string {
	return []string{"host", "x-amz-content-sha256", "x-amz-date", "x-amz-decoded-content-length", "x-amz-trailer"}
}

func mustParse(p string) *url.URL {
	u, err := url.Parse(p)
	if err != nil {
		panic(err)
	}
	return u
}
