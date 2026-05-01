package s3api_test

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/danchupin/strata/internal/auth"
	datamem "github.com/danchupin/strata/internal/data/memory"
	metamem "github.com/danchupin/strata/internal/meta/memory"
	"github.com/danchupin/strata/internal/s3api"
)

// SigV4 streaming constants — duplicated locally because internal/auth keeps
// them unexported. Values are AWS-spec constants and will not change.
const (
	streamingTestAlgo     = "AWS4-HMAC-SHA256"
	streamingTestTerm     = "aws4_request"
	streamingTestRegion   = "us-east-1"
	streamingTestService  = "s3"
	streamingTestTimeFmt  = "20060102T150405Z"
	streamingTestSentinel = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD"
	streamingTestEmptyHex = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	streamingTestAK       = "AKIDEXAMPLE"
	streamingTestSecret   = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
)

func sigHmacTest(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func sigSha256HexTest(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func deriveSigningKeyTest(secret, date, region, service string) []byte {
	k := sigHmacTest([]byte("AWS4"+secret), []byte(date))
	k = sigHmacTest(k, []byte(region))
	k = sigHmacTest(k, []byte(service))
	return sigHmacTest(k, []byte(streamingTestTerm))
}

// chainSigTest computes one per-chunk chain signature.
func chainSigTest(signingKey []byte, isoDate, scope, prevSig string, payload []byte) string {
	sts := "AWS4-HMAC-SHA256-PAYLOAD\n" +
		isoDate + "\n" +
		scope + "\n" +
		prevSig + "\n" +
		streamingTestEmptyHex + "\n" +
		sigSha256HexTest(payload)
	return hex.EncodeToString(sigHmacTest(signingKey, []byte(sts)))
}

// signedHarness wraps s3api.Server with auth.Middleware{ModeRequired} so
// the streaming-SigV4 chain validator runs end-to-end. Every request must
// be signed with streamingTestAK / streamingTestSecret.
type signedHarness struct {
	t  *testing.T
	ts *httptest.Server
}

func newSignedHarness(t *testing.T) *signedHarness {
	t.Helper()
	api := s3api.New(datamem.New(), metamem.New())
	api.Region = streamingTestRegion
	mw := &auth.Middleware{
		Store: auth.NewStaticStore(map[string]*auth.Credential{
			streamingTestAK: {AccessKey: streamingTestAK, Secret: streamingTestSecret, Owner: "test"},
		}),
		Mode: auth.ModeRequired,
	}
	ts := httptest.NewServer(mw.Wrap(api, s3api.WriteAuthDenied))
	t.Cleanup(ts.Close)
	return &signedHarness{t: t, ts: ts}
}

// signRequest fills Authorization + X-Amz-Date + X-Amz-Content-Sha256 on
// req using the canonical-request shape that internal/auth/sigv4.go's
// validateHeader expects. bodyHash must already be the desired
// X-Amz-Content-Sha256 value (sha256 hex of the body OR a streaming
// sentinel). signedHeaders is sorted in place to keep the signature
// reproducible across callers.
func (h *signedHarness) signRequest(req *http.Request, bodyHash string, signedHeaders []string, extra map[string]string) {
	h.t.Helper()
	now := time.Now().UTC().Format(streamingTestTimeFmt)
	day := now[:8]
	scope := day + "/" + streamingTestRegion + "/" + streamingTestService + "/" + streamingTestTerm

	req.Header.Set("X-Amz-Date", now)
	req.Header.Set("X-Amz-Content-Sha256", bodyHash)
	for k, v := range extra {
		req.Header.Set(k, v)
	}

	sort.Strings(signedHeaders)

	var canonicalHeadersBuf strings.Builder
	for _, name := range signedHeaders {
		canonicalHeadersBuf.WriteString(name)
		canonicalHeadersBuf.WriteByte(':')
		var val string
		if name == "host" {
			val = req.Host
		} else {
			val = strings.TrimSpace(req.Header.Get(name))
		}
		canonicalHeadersBuf.WriteString(val)
		canonicalHeadersBuf.WriteByte('\n')
	}

	canonicalReq := req.Method + "\n" +
		req.URL.EscapedPath() + "\n" +
		"" + "\n" + // no query in test paths
		canonicalHeadersBuf.String() + "\n" +
		strings.Join(signedHeaders, ";") + "\n" +
		bodyHash

	sts := streamingTestAlgo + "\n" + now + "\n" + scope + "\n" + sigSha256HexTest([]byte(canonicalReq))
	signingKey := deriveSigningKeyTest(streamingTestSecret, day, streamingTestRegion, streamingTestService)
	seedSig := hex.EncodeToString(sigHmacTest(signingKey, []byte(sts)))

	req.Header.Set("Authorization", fmt.Sprintf(
		"%s Credential=%s/%s/%s/%s/%s, SignedHeaders=%s, Signature=%s",
		streamingTestAlgo, streamingTestAK, day, streamingTestRegion, streamingTestService,
		streamingTestTerm, strings.Join(signedHeaders, ";"), seedSig,
	))
}

// putBucketSigned sends a SigV4-signed PUT /<bucket> with empty body.
func (h *signedHarness) putBucketSigned(bucket string) *http.Response {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodPut, h.ts.URL+"/"+bucket, nil)
	if err != nil {
		h.t.Fatalf("new bucket request: %v", err)
	}
	h.signRequest(req, streamingTestEmptyHex, []string{"host", "x-amz-content-sha256", "x-amz-date"}, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("put bucket: %v", err)
	}
	return resp
}

// getObjectSigned sends a SigV4-signed GET /<bucket>/<key>.
func (h *signedHarness) getObjectSigned(bucket, key string) *http.Response {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodGet, h.ts.URL+"/"+bucket+"/"+key, nil)
	if err != nil {
		h.t.Fatalf("new get request: %v", err)
	}
	h.signRequest(req, streamingTestEmptyHex, []string{"host", "x-amz-content-sha256", "x-amz-date"}, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("get object: %v", err)
	}
	return resp
}

// putStreamingObject builds a 3-chunk aws-chunked PUT (chunkA, chunkB,
// final-empty) with valid chain signatures and sends it. If mutateAt >= 0,
// the byte at that absolute offset inside the assembled chunked body is
// XOR'd with 0x01 BEFORE sending — simulating an in-flight mutation. The
// chunk-signature header itself is left untouched so the server-side chain
// validator catches the payload change.
func (h *signedHarness) putStreamingObject(bucket, key string, chunkA, chunkB []byte, mutateAt int) *http.Response {
	h.t.Helper()
	now := time.Now().UTC().Format(streamingTestTimeFmt)
	day := now[:8]
	scope := day + "/" + streamingTestRegion + "/" + streamingTestService + "/" + streamingTestTerm
	signingKey := deriveSigningKeyTest(streamingTestSecret, day, streamingTestRegion, streamingTestService)

	decodedLen := len(chunkA) + len(chunkB)
	decodedStr := fmt.Sprintf("%d", decodedLen)

	// Build a placeholder request to grab req.Host, then sign with a
	// fixed signed-header set that includes the streaming-required ones.
	req, err := http.NewRequest(http.MethodPut, h.ts.URL+"/"+bucket+"/"+key, nil)
	if err != nil {
		h.t.Fatalf("new put request: %v", err)
	}
	req.Header.Set("X-Amz-Date", now)
	req.Header.Set("X-Amz-Content-Sha256", streamingTestSentinel)
	req.Header.Set("Content-Encoding", "aws-chunked")
	req.Header.Set("X-Amz-Decoded-Content-Length", decodedStr)

	signedHeaders := []string{
		"content-encoding",
		"host",
		"x-amz-content-sha256",
		"x-amz-date",
		"x-amz-decoded-content-length",
	}
	sort.Strings(signedHeaders)

	var canonicalHeadersBuf strings.Builder
	for _, name := range signedHeaders {
		canonicalHeadersBuf.WriteString(name)
		canonicalHeadersBuf.WriteByte(':')
		var val string
		if name == "host" {
			val = req.Host
		} else {
			val = strings.TrimSpace(req.Header.Get(name))
		}
		canonicalHeadersBuf.WriteString(val)
		canonicalHeadersBuf.WriteByte('\n')
	}
	canonicalReq := req.Method + "\n" +
		req.URL.EscapedPath() + "\n" +
		"" + "\n" +
		canonicalHeadersBuf.String() + "\n" +
		strings.Join(signedHeaders, ";") + "\n" +
		streamingTestSentinel
	sts := streamingTestAlgo + "\n" + now + "\n" + scope + "\n" + sigSha256HexTest([]byte(canonicalReq))
	seedSig := hex.EncodeToString(sigHmacTest(signingKey, []byte(sts)))

	req.Header.Set("Authorization", fmt.Sprintf(
		"%s Credential=%s/%s/%s/%s/%s, SignedHeaders=%s, Signature=%s",
		streamingTestAlgo, streamingTestAK, day, streamingTestRegion, streamingTestService,
		streamingTestTerm, strings.Join(signedHeaders, ";"), seedSig,
	))

	// Compose chunked body with valid chain signatures.
	var body bytes.Buffer
	prev := seedSig
	for _, p := range [][]byte{chunkA, chunkB, {}} {
		sig := chainSigTest(signingKey, now, scope, prev, p)
		prev = sig
		fmt.Fprintf(&body, "%x;chunk-signature=%s\r\n", len(p), sig)
		body.Write(p)
		body.WriteString("\r\n")
	}
	raw := body.Bytes()
	if mutateAt >= 0 {
		if mutateAt >= len(raw) {
			h.t.Fatalf("mutateAt=%d out of range (body len=%d)", mutateAt, len(raw))
		}
		raw[mutateAt] ^= 0x01
	}

	req.Body = io.NopCloser(bytes.NewReader(raw))
	req.ContentLength = int64(len(raw))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("streaming put: %v", err)
	}
	return resp
}

// TestStreamingSigV4_PristineUpload_Succeeds — Variant A from US-004 AC:
// a properly-signed streaming PUT with valid chain signatures returns 200,
// and a subsequent GET returns 200 with the full reconstructed body. This
// proves the chain validator does not over-reject pristine uploads.
func TestStreamingSigV4_PristineUpload_Succeeds(t *testing.T) {
	h := newSignedHarness(t)
	resp := h.putBucketSigned("bkt")
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Fatalf("put bucket: status=%d body=%s", resp.StatusCode, body)
	}
	_ = resp.Body.Close()

	chunkA := []byte("alpha-chunk-0123456789")
	chunkB := []byte("bravo-chunk-abcdefghij")
	resp = h.putStreamingObject("bkt", "obj", chunkA, chunkB, -1)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Fatalf("pristine streaming put: status=%d body=%s", resp.StatusCode, body)
	}
	_ = resp.Body.Close()

	resp = h.getObjectSigned("bkt", "obj")
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Fatalf("get object after pristine put: status=%d body=%s", resp.StatusCode, body)
	}
	got, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("read get body: %v", err)
	}
	want := append(append([]byte{}, chunkA...), chunkB...)
	if !bytes.Equal(got, want) {
		t.Fatalf("get body mismatch\n  got  %q\n  want %q", got, want)
	}
}

// TestStreamingSigV4_MutatedChunk_Rejected — Variant B from US-004 AC:
// an attacker mutating one byte of chunk-2's payload mid-stream causes the
// PUT to return 403 SignatureDoesNotMatch AND the subsequent GET returns
// 404 NoSuchKey, proving the buffer-then-validate guarantee — mutated
// bytes never reach the storage backend.
func TestStreamingSigV4_MutatedChunk_Rejected(t *testing.T) {
	h := newSignedHarness(t)
	resp := h.putBucketSigned("bkt")
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Fatalf("put bucket: status=%d body=%s", resp.StatusCode, body)
	}
	_ = resp.Body.Close()

	chunkA := []byte("alpha-chunk-0123456789")
	chunkB := []byte("bravo-chunk-abcdefghij")

	// Find chunk-2's payload offset in the assembled body so we can mutate
	// one byte inside it. Build a pristine body first to locate it.
	var pristine bytes.Buffer
	{
		now := time.Now().UTC().Format(streamingTestTimeFmt)
		day := now[:8]
		scope := day + "/" + streamingTestRegion + "/" + streamingTestService + "/" + streamingTestTerm
		signingKey := deriveSigningKeyTest(streamingTestSecret, day, streamingTestRegion, streamingTestService)
		// Use a placeholder seedSig; the offset of chunkB inside the assembled
		// body does not depend on seedSig because the size header for chunk-1
		// has fixed width once chunkA's len is fixed.
		prev := strings.Repeat("a", 64)
		for _, p := range [][]byte{chunkA, chunkB, {}} {
			sig := chainSigTest(signingKey, now, scope, prev, p)
			prev = sig
			fmt.Fprintf(&pristine, "%x;chunk-signature=%s\r\n", len(p), sig)
			pristine.Write(p)
			pristine.WriteString("\r\n")
		}
	}
	idx := bytes.Index(pristine.Bytes(), chunkB)
	if idx < 0 {
		t.Fatalf("could not locate chunkB inside assembled body")
	}
	mutateAt := idx + 3

	resp = h.putStreamingObject("bkt", "obj", chunkA, chunkB, mutateAt)
	if resp.StatusCode != http.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Fatalf("mutated streaming put: status=%d (want 403) body=%s", resp.StatusCode, body)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "SignatureDoesNotMatch") {
		t.Fatalf("mutated put body missing SignatureDoesNotMatch: %s", body)
	}

	// GET must return 404 NoSuchKey — the mutated bytes never reached the
	// meta store, so the object does not exist.
	resp = h.getObjectSigned("bkt", "obj")
	if resp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Fatalf("get after mutated put: status=%d (want 404) body=%s", resp.StatusCode, body)
	}
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "NoSuchKey") {
		t.Fatalf("get after mutated put: body missing NoSuchKey: %s", body)
	}
}
