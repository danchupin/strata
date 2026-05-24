package auth

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// sha256TrailerSpec is the default sha256 spec used by the trailer-decoder
// tests; per-algorithm tests build their own spec via selectTrailerHash.
func sha256TrailerSpec() *trailerHashSpec {
	spec, err := selectTrailerHash(trailerHeaderChecksumSha256)
	if err != nil {
		panic(err)
	}
	return spec
}

// awsStreamingTrailerBody hand-constructs a STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER
// body for the given chunks under the test signing key, with a trailing
// x-amz-checksum-sha256 header. trailerB64 lets the caller forge a checksum
// mismatch (pass "" for happy-path = real sha256). forgeTrailerSig overrides
// the trailer signature bytes — used for the trailer-sig-mismatch leg.
func awsStreamingTrailerBody(chunks [][]byte, trailerB64 string, forgeTrailerSig string) (body, plain []byte) {
	return awsStreamingTrailerBodyAlgo(chunks, trailerHeaderChecksumSha256, sha256.New, trailerB64, forgeTrailerSig)
}

// awsStreamingTrailerBodyAlgo is the per-algorithm generalisation of
// awsStreamingTrailerBody. It accepts the canonical trailer header name and
// the hash.Hash constructor used to derive the body checksum so the same
// helper produces fixtures for sha256 / sha1 / crc32 / crc32c.
func awsStreamingTrailerBodyAlgo(chunks [][]byte, header string, newH func() hash.Hash, trailerB64 string, forgeTrailerSig string) (body, plain []byte) {
	key := streamSigningKey()
	scope := streamScope()
	prev := streamSeedSig
	for _, c := range chunks {
		sig := computeChunkSignature(key, streamTimestamp, scope, prev, c)
		body = append(body, encodeChunk(c, sig)...)
		plain = append(plain, c...)
		prev = sig
	}
	finalSig := computeChunkSignature(key, streamTimestamp, scope, prev, nil)
	body = append(body, encodeChunk(nil, finalSig)...)

	if trailerB64 == "" {
		h := newH()
		h.Write(plain)
		trailerB64 = base64.StdEncoding.EncodeToString(h.Sum(nil))
	}
	canonical := header + ":" + trailerB64 + "\n"
	trailerSig := computeTrailerSignature(key, streamTimestamp, scope, finalSig, canonical)
	if forgeTrailerSig != "" {
		trailerSig = forgeTrailerSig
	}
	body = append(body, []byte(header+":"+trailerB64+"\r\n")...)
	body = append(body, []byte("x-amz-trailer-signature:"+trailerSig+"\r\n")...)
	body = append(body, []byte("\r\n")...)
	return body, plain
}

func TestStreamingTrailerReaderPositive(t *testing.T) {
	body, plain := awsStreamingTrailerBody([][]byte{
		bytes.Repeat([]byte{'a'}, 65536),
		bytes.Repeat([]byte{'b'}, 1024),
	}, "", "")
	r := newStreamingTrailerReader(io.NopCloser(bytes.NewReader(body)), streamSigningKey(), streamTimestamp, streamScope(), streamSeedSig, sha256TrailerSpec())
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("decoded body mismatch: got %d bytes, want %d", len(got), len(plain))
	}
}

// TestStreamingTrailerReaderGoldenFixture decodes the committed
// testdata/chunked-trailer-<algo>.bin fixtures (sha256 from US-009; sha1 /
// crc32 / crc32c from US-004). The fixtures are generated from the same
// chained-HMAC helpers (awsStreamingTrailerBodyAlgo) under the deterministic
// streamSecret / streamSeedSig vector, so this test double-guards (a) the
// binary fixtures stay in sync with the decoder and (b) the decoder
// correctly recovers the documented plaintext for every supported algo.
func TestStreamingTrailerReaderGoldenFixture(t *testing.T) {
	chunks := [][]byte{
		bytes.Repeat([]byte("Hello, US-009 trailer-aware streaming. "), 1024),
		bytes.Repeat([]byte("Second chunk "), 64),
	}
	var wantPlain []byte
	for _, c := range chunks {
		wantPlain = append(wantPlain, c...)
	}

	for _, header := range []string{
		trailerHeaderChecksumSha256,
		trailerHeaderChecksumSha1,
		trailerHeaderChecksumCRC32,
		trailerHeaderChecksumCRC32C,
	} {
		t.Run(header, func(t *testing.T) {
			path := "testdata/chunked-trailer-" + strings.TrimPrefix(header, "x-amz-checksum-") + ".bin"
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read fixture %s: %v", path, err)
			}
			spec, err := selectTrailerHash(header)
			if err != nil {
				t.Fatalf("selectTrailerHash(%q): %v", header, err)
			}
			r := newStreamingTrailerReader(io.NopCloser(bytes.NewReader(body)), streamSigningKey(), streamTimestamp, streamScope(), streamSeedSig, spec)
			got, err := io.ReadAll(r)
			if err != nil {
				t.Fatalf("read %s body: %v", path, err)
			}
			if !bytes.Equal(got, wantPlain) {
				t.Fatalf("%s fixture plaintext mismatch: got %d bytes, want %d", path, len(got), len(wantPlain))
			}
		})
	}
}

func TestStreamingTrailerReaderTrailerSignatureMismatchRejected(t *testing.T) {
	// 64 zeroes => same hex length but never matches a real HMAC.
	body, _ := awsStreamingTrailerBody([][]byte{
		bytes.Repeat([]byte{'a'}, 32),
	}, "", strings.Repeat("0", 64))
	r := newStreamingTrailerReader(io.NopCloser(bytes.NewReader(body)), streamSigningKey(), streamTimestamp, streamScope(), streamSeedSig, sha256TrailerSpec())
	_, err := io.ReadAll(r)
	if !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("expected ErrSignatureInvalid on trailer-sig mismatch, got %v", err)
	}
}

func TestStreamingTrailerReaderBodyChecksumMismatchRejected(t *testing.T) {
	// Forge a base64 checksum that doesn't match sha256(plain) — trailer
	// signature is computed over the forged value (so its signature
	// validates), but the body sha256 comparison must reject.
	body, _ := awsStreamingTrailerBody([][]byte{
		bytes.Repeat([]byte{'a'}, 32),
	}, base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0}, sha256.Size)), "")
	r := newStreamingTrailerReader(io.NopCloser(bytes.NewReader(body)), streamSigningKey(), streamTimestamp, streamScope(), streamSeedSig, sha256TrailerSpec())
	_, err := io.ReadAll(r)
	if !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("expected ErrSignatureInvalid on body-checksum mismatch, got %v", err)
	}
}

func TestStreamingTrailerReaderMissingTrailerSignature(t *testing.T) {
	// Hand-build a trailer block that drops the trailer-signature header.
	chunks := [][]byte{bytes.Repeat([]byte{'a'}, 16)}
	key := streamSigningKey()
	scope := streamScope()
	prev := streamSeedSig
	var body, plain []byte
	for _, c := range chunks {
		sig := computeChunkSignature(key, streamTimestamp, scope, prev, c)
		body = append(body, encodeChunk(c, sig)...)
		plain = append(plain, c...)
		prev = sig
	}
	finalSig := computeChunkSignature(key, streamTimestamp, scope, prev, nil)
	body = append(body, encodeChunk(nil, finalSig)...)
	sum := sha256.Sum256(plain)
	b64 := base64.StdEncoding.EncodeToString(sum[:])
	body = append(body, []byte("x-amz-checksum-sha256:"+b64+"\r\n")...)
	body = append(body, []byte("\r\n")...)

	r := newStreamingTrailerReader(io.NopCloser(bytes.NewReader(body)), key, streamTimestamp, streamScope(), streamSeedSig, sha256TrailerSpec())
	_, err := io.ReadAll(r)
	if !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("expected ErrSignatureInvalid on missing trailer signature, got %v", err)
	}
}

func TestStreamingTrailerReaderMissingChecksumHeader(t *testing.T) {
	// Trailer block with signature header but no checksum header.
	chunks := [][]byte{bytes.Repeat([]byte{'a'}, 16)}
	key := streamSigningKey()
	scope := streamScope()
	prev := streamSeedSig
	var body []byte
	for _, c := range chunks {
		sig := computeChunkSignature(key, streamTimestamp, scope, prev, c)
		body = append(body, encodeChunk(c, sig)...)
		prev = sig
	}
	finalSig := computeChunkSignature(key, streamTimestamp, scope, prev, nil)
	body = append(body, encodeChunk(nil, finalSig)...)
	// Compute trailer-sig over empty canonical to satisfy the parser if
	// it relaxes — but the decoder must still reject for the missing
	// checksum semantic.
	trailerSig := computeTrailerSignature(key, streamTimestamp, scope, finalSig, "")
	body = append(body, []byte("x-amz-trailer-signature:"+trailerSig+"\r\n")...)
	body = append(body, []byte("\r\n")...)

	r := newStreamingTrailerReader(io.NopCloser(bytes.NewReader(body)), key, streamTimestamp, streamScope(), streamSeedSig, sha256TrailerSpec())
	_, err := io.ReadAll(r)
	if !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("expected ErrSignatureInvalid on missing checksum header, got %v", err)
	}
}

// TestMiddlewareSupportedTrailerAlgos exercises the trailer-mode gate in
// validateHeader. The 4 algos shipped by US-004 (sha256 / sha1 / crc32 /
// crc32c) MUST pass selectTrailerHash and let the request proceed past
// validation. An unknown algo (e.g. `x-amz-checksum-md5`) MUST surface
// ErrUnsupportedChecksumAlgorithm so the gateway can reject with HTTP 400.
func TestMiddlewareSupportedTrailerAlgos(t *testing.T) {
	store := newTrailerTestStore()
	m := &Middleware{Store: store, Mode: ModeRequired}

	signedHeaders := []string{"host", "x-amz-content-sha256", "x-amz-date", "x-amz-trailer"}
	for _, algo := range []string{
		"x-amz-checksum-sha256",
		"x-amz-checksum-sha1",
		"x-amz-checksum-crc32",
		"x-amz-checksum-crc32c",
	} {
		req := newSignedTrailerRequest(t, store, algo, signedHeaders)
		if _, err := m.validate(req); err != nil {
			t.Fatalf("algo %s: expected pass-through, got %v", algo, err)
		}
	}

	// Unsupported (md5) still rejects.
	req := newSignedTrailerRequest(t, store, "x-amz-checksum-md5", signedHeaders)
	if _, err := m.validate(req); !errors.Is(err, ErrUnsupportedChecksumAlgorithm) {
		t.Fatalf("md5: want ErrUnsupportedChecksumAlgorithm, got %v", err)
	}
}

// TestStreamingTrailerReaderAllAlgos exercises the per-algorithm decoder
// loop for the 4 supported trailer checksum algos: happy-path round-trip
// (b) body-checksum mismatch surfaces ErrSignatureInvalid.
func TestStreamingTrailerReaderAllAlgos(t *testing.T) {
	type algo struct {
		name      string
		header    string
		newH      func() hash.Hash
		zeroBytes int
	}
	cases := []algo{
		{name: "sha256", header: trailerHeaderChecksumSha256, newH: sha256.New, zeroBytes: sha256.Size},
		{name: "sha1", header: trailerHeaderChecksumSha1, newH: sha1.New, zeroBytes: sha1.Size},
		{name: "crc32", header: trailerHeaderChecksumCRC32, newH: func() hash.Hash { return crc32.NewIEEE() }, zeroBytes: 4},
		{name: "crc32c", header: trailerHeaderChecksumCRC32C, newH: func() hash.Hash { return crc32.New(crc32CastagnoliTable) }, zeroBytes: 4},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name+"/positive", func(t *testing.T) {
			body, plain := awsStreamingTrailerBodyAlgo([][]byte{
				bytes.Repeat([]byte{'a'}, 4096),
				bytes.Repeat([]byte{'b'}, 1024),
			}, c.header, c.newH, "", "")
			spec, err := selectTrailerHash(c.header)
			if err != nil {
				t.Fatalf("selectTrailerHash(%q): %v", c.header, err)
			}
			r := newStreamingTrailerReader(io.NopCloser(bytes.NewReader(body)), streamSigningKey(), streamTimestamp, streamScope(), streamSeedSig, spec)
			got, err := io.ReadAll(r)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if !bytes.Equal(got, plain) {
				t.Fatalf("plain mismatch: got %d, want %d", len(got), len(plain))
			}
		})

		t.Run(c.name+"/mismatch", func(t *testing.T) {
			body, _ := awsStreamingTrailerBodyAlgo([][]byte{
				bytes.Repeat([]byte{'a'}, 64),
			}, c.header, c.newH, base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0}, c.zeroBytes)), "")
			spec, err := selectTrailerHash(c.header)
			if err != nil {
				t.Fatalf("selectTrailerHash(%q): %v", c.header, err)
			}
			r := newStreamingTrailerReader(io.NopCloser(bytes.NewReader(body)), streamSigningKey(), streamTimestamp, streamScope(), streamSeedSig, spec)
			if _, err := io.ReadAll(r); !errors.Is(err, ErrSignatureInvalid) {
				t.Fatalf("expected ErrSignatureInvalid on %s body-checksum mismatch, got %v", c.name, err)
			}
		})
	}
}

// TestSelectTrailerHashUnsupported guards the wire-format gate: anything
// outside the known set surfaces ErrUnsupportedChecksumAlgorithm.
func TestSelectTrailerHashUnsupported(t *testing.T) {
	for _, algo := range []string{
		"",
		"x-amz-checksum-md5",
		"x-amz-checksum-sha512",
		"foo",
	} {
		if _, err := selectTrailerHash(algo); !errors.Is(err, ErrUnsupportedChecksumAlgorithm) {
			t.Fatalf("selectTrailerHash(%q): want ErrUnsupportedChecksumAlgorithm, got %v", algo, err)
		}
	}
}

// --- helpers for the middleware-level trailer test ---

type trailerTestStore struct {
	cred Credential
}

func newTrailerTestStore() *trailerTestStore {
	return &trailerTestStore{cred: Credential{AccessKey: "AKIAIOSFODNN7EXAMPLE", Secret: streamSecret, Owner: "tester"}}
}

func (s *trailerTestStore) Lookup(_ context.Context, ak string) (*Credential, error) {
	if ak != s.cred.AccessKey {
		return nil, ErrNoSuchCredential
	}
	c := s.cred
	return &c, nil
}

func newSignedTrailerRequest(t *testing.T, store *trailerTestStore, algo string, signedHeaders []string) *http.Request {
	t.Helper()
	// Body shape is irrelevant — the unsupported-algo gate rejects before
	// the body is read.
	req := httptestNewReq("PUT", "http://example.com/b/k", strings.NewReader(""))
	req.Host = "example.com"
	req.Header.Set("X-Amz-Content-Sha256", streamingBodyTrailer)
	req.Header.Set("X-Amz-Trailer", algo)
	req.Header.Set("X-Amz-Date", streamTimestamp)
	req.Header.Set("X-Amz-Decoded-Content-Length", "0")
	// Use a recent timestamp so the skew check passes.
	req = withFreshTimestamp(req)

	parsed := &parsedAuth{
		AccessKey:     store.cred.AccessKey,
		Date:          headerDate(req),
		Region:        streamRegion,
		Service:       streamService,
		SignedHeaders: signedHeaders,
		Signature:     "",
	}
	canonical := canonicalRequest(req, parsed.SignedHeaders, streamingBodyTrailer)
	sig := computeSignature(store.cred.Secret, parsed, req.Header.Get("X-Amz-Date"), canonical)

	authHeader := fmt.Sprintf("%s Credential=%s/%s/%s/%s/%s, SignedHeaders=%s, Signature=%s",
		sigAlgorithm,
		parsed.AccessKey, parsed.Date, parsed.Region, parsed.Service, sigTerminator,
		strings.Join(parsed.SignedHeaders, ";"),
		sig,
	)
	req.Header.Set("Authorization", authHeader)
	return req
}

// httptestNewReq centralises the request constructor so the import block in
// streaming_trailer_test.go stays small.
func httptestNewReq(method, url string, body io.Reader) *http.Request {
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		panic(err)
	}
	return req
}

// withFreshTimestamp rewrites the X-Amz-Date header to a time within the
// max-skew window so middleware.validateHeader does not reject on age.
// streamTimestamp is "20130524T000000Z" — far outside the window.
func withFreshTimestamp(req *http.Request) *http.Request {
	req.Header.Set("X-Amz-Date", time.Now().UTC().Format(sigTimeFormat))
	return req
}

func headerDate(req *http.Request) string {
	d := req.Header.Get("X-Amz-Date")
	if len(d) < 8 {
		return ""
	}
	return d[:8]
}
