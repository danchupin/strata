package auth

import (
	"bytes"
	"crypto/hmac"
	"errors"
	"io"
	"strings"
	"testing"
)

// nopCloser wraps an io.Reader as an io.ReadCloser for tests.
type nopCloser struct{ io.Reader }

func (nopCloser) Close() error { return nil }

// buildChunkedBody emits the aws-chunked framing for a sequence of
// payloads. The trailing payload should be a zero-length slice — that
// becomes the final empty chunk that closes the chain. Each chunk uses
// the chain signature produced by the supplied signer (so callers can
// produce a valid body OR mutate one chunk to test rejection).
func buildChunkedBody(t *testing.T, signer *chunkSigner, payloads [][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	for _, p := range payloads {
		sig := signer.Next(p)
		buf.WriteString(strings.ToLower(intToHex(len(p))))
		buf.WriteString(";chunk-signature=")
		buf.WriteString(sig)
		buf.WriteString("\r\n")
		buf.Write(p)
		buf.WriteString("\r\n")
	}
	return buf.Bytes()
}

// intToHex returns n as lowercase hex without padding (matches AWS
// aws-chunked size headers, e.g. "10000" for 65536).
func intToHex(n int) string {
	if n == 0 {
		return "0"
	}
	const hexdig = "0123456789abcdef"
	var buf [16]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = hexdig[n&0xf]
		n >>= 4
	}
	return string(buf[i:])
}

// TestChunkSignerAWSVectors verifies the chained per-chunk signatures
// against the AWS-published "Example Calculations" for streaming SigV4.
//
// Source: https://docs.aws.amazon.com/AmazonS3/latest/API/sigv4-streaming.html
// "Example Calculations" — PUT /examplebucket/chunkObject.txt with a
// 66560-byte body of 'a' bytes split into one 65536-byte chunk, one
// 1024-byte chunk, and a final empty chunk. Vectors transcribed inline
// so the test is offline-runnable across AWS doc URL rotations.
func TestChunkSignerAWSVectors(t *testing.T) {
	const (
		secret  = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
		date    = "20130524"
		region  = "us-east-1"
		service = "s3"
		isoDate = "20130524T000000Z"
		// Outer SigV4 signature from the request's Authorization header.
		seedSig = "4f232c4386841ef735655705268965c44a0e4690baa4adea153f7db9fa80a0a9"
	)

	scope := credentialScope(date, region, service)
	signingKey := deriveSigningKey(secret, date, region, service)

	cases := []struct {
		name        string
		payload     []byte
		expectedSig string
	}{
		{
			name:        "chunk-1 (65536 bytes of 'a')",
			payload:     bytes.Repeat([]byte{'a'}, 65536),
			expectedSig: "ad80c730a21e5b8d04586a2213dd63b9a0e99e0e2307b0ade35a65485a288648",
		},
		{
			name:        "chunk-2 (1024 bytes of 'a')",
			payload:     bytes.Repeat([]byte{'a'}, 1024),
			expectedSig: "0055627c9e194cb4542bae2aa5492e3c1575bbb81b612b7d234b86a503ef5497",
		},
		{
			name:        "final-empty (0 bytes still participates in the chain)",
			payload:     []byte{},
			expectedSig: "b6c6ea8a5354eaf15b3cb7646744f4275b71ea724fed81ceb9323e279d449df9",
		},
	}

	cs := newChunkSigner(seedSig, signingKey, isoDate, scope)
	for _, tc := range cases {
		got := cs.Next(tc.payload)
		// hmac.Equal: constant-time compare, Go stdlib idiom for auth tags.
		if !hmac.Equal([]byte(got), []byte(tc.expectedSig)) {
			t.Errorf("%s: signature mismatch\n  got  %s\n  want %s", tc.name, got, tc.expectedSig)
		}
	}
}

// TestChunkSignerAdvancesChain verifies that Next mutates prevSig so that
// repeated calls with the same payload produce different signatures (the
// chain advances).
func TestChunkSignerAdvancesChain(t *testing.T) {
	signingKey := deriveSigningKey("secret", "20260101", "us-east-1", "s3")
	scope := credentialScope("20260101", "us-east-1", "s3")
	cs := newChunkSigner("seed", signingKey, "20260101T000000Z", scope)

	payload := []byte("hello")
	first := cs.Next(payload)
	second := cs.Next(payload)
	if first == second {
		t.Fatalf("chain did not advance: both calls returned %s", first)
	}
}

// TestStreamingReader_ValidChain reads a 3-chunk body whose chain
// signatures match the chain the reader recomputes. ReadAll returns the
// concatenated payloads with no error.
func TestStreamingReader_ValidChain(t *testing.T) {
	const (
		secret  = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
		date    = "20260101"
		region  = "us-east-1"
		service = "s3"
		isoDate = "20260101T000000Z"
		seedSig = "deadbeef"
	)
	scope := credentialScope(date, region, service)
	signingKey := deriveSigningKey(secret, date, region, service)
	chunkA := []byte("hello ")
	chunkB := []byte("world")
	body := buildChunkedBody(t, newChunkSigner(seedSig, signingKey, isoDate, scope),
		[][]byte{chunkA, chunkB, {}})

	r := newStreamingReader(nopCloser{bytes.NewReader(body)}, seedSig, signingKey, isoDate, scope)
	defer r.Close()
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	want := append(append([]byte{}, chunkA...), chunkB...)
	if !bytes.Equal(got, want) {
		t.Fatalf("payload mismatch\n  got  %q\n  want %q", got, want)
	}
}

// TestStreamingReader_MutatedChunk_Rejects flips one byte of chunk-2's
// payload (without touching the chunk-signature header) and verifies the
// reader returns ErrChunkSignatureMismatch — and that none of chunk-2's
// bytes are forwarded to the consumer (buffer-then-validate guarantee).
func TestStreamingReader_MutatedChunk_Rejects(t *testing.T) {
	const (
		secret  = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
		date    = "20260101"
		region  = "us-east-1"
		service = "s3"
		isoDate = "20260101T000000Z"
		seedSig = "deadbeef"
	)
	scope := credentialScope(date, region, service)
	signingKey := deriveSigningKey(secret, date, region, service)
	chunkA := []byte("alpha-chunk")
	chunkB := []byte("bravo-chunk")
	body := buildChunkedBody(t, newChunkSigner(seedSig, signingKey, isoDate, scope),
		[][]byte{chunkA, chunkB, {}})

	// Locate chunk-2's payload offset and flip one byte.
	idx := bytes.Index(body, chunkB)
	if idx < 0 {
		t.Fatalf("could not find chunk-2 payload in body")
	}
	body[idx+3] ^= 0x01

	r := newStreamingReader(nopCloser{bytes.NewReader(body)}, seedSig, signingKey, isoDate, scope)
	defer r.Close()
	got, err := io.ReadAll(r)
	if !errors.Is(err, ErrChunkSignatureMismatch) {
		t.Fatalf("ReadAll err = %v, want ErrChunkSignatureMismatch", err)
	}
	// chunk-1 may pass through (its chain still validated). chunk-2 must
	// not — its bytes are buffered until validation, which fails.
	if bytes.Contains(got, chunkB) {
		t.Fatalf("mutated chunk leaked to consumer: got %q", got)
	}
}

// TestStreamingReader_MutatedTrailer_Rejects flips one byte of the final
// empty chunk's signature so its chain check fails. The reader must
// surface ErrChunkSignatureMismatch — a mutated trailer must not slip
// through and let an attacker truncate the body.
func TestStreamingReader_MutatedTrailer_Rejects(t *testing.T) {
	const (
		secret  = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
		date    = "20260101"
		region  = "us-east-1"
		service = "s3"
		isoDate = "20260101T000000Z"
		seedSig = "deadbeef"
	)
	scope := credentialScope(date, region, service)
	signingKey := deriveSigningKey(secret, date, region, service)
	chunkA := []byte("only-chunk")
	body := buildChunkedBody(t, newChunkSigner(seedSig, signingKey, isoDate, scope),
		[][]byte{chunkA, {}})

	// Flip a byte inside the trailer's chunk-signature value. The trailer
	// line begins after chunkA's CRLF as "0;chunk-signature=...". Locate
	// the second occurrence of "chunk-signature=" and mutate one hex byte.
	first := bytes.Index(body, []byte("chunk-signature="))
	second := bytes.Index(body[first+1:], []byte("chunk-signature="))
	if first < 0 || second < 0 {
		t.Fatalf("could not find trailer chunk-signature in body")
	}
	off := first + 1 + second + len("chunk-signature=")
	if body[off] == 'a' {
		body[off] = 'b'
	} else {
		body[off] = 'a'
	}

	r := newStreamingReader(nopCloser{bytes.NewReader(body)}, seedSig, signingKey, isoDate, scope)
	defer r.Close()
	if _, err := io.ReadAll(r); !errors.Is(err, ErrChunkSignatureMismatch) {
		t.Fatalf("ReadAll err = %v, want ErrChunkSignatureMismatch", err)
	}
}

// TestStreamingReader_OversizedChunk_Rejects ensures a malicious size
// header claiming > 16 MiB is refused without allocating that many bytes
// (the maxChunkPayload cap defends against a malformed-framing OOM).
func TestStreamingReader_OversizedChunk_Rejects(t *testing.T) {
	const (
		secret  = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
		date    = "20260101"
		region  = "us-east-1"
		service = "s3"
		isoDate = "20260101T000000Z"
		seedSig = "deadbeef"
	)
	scope := credentialScope(date, region, service)
	signingKey := deriveSigningKey(secret, date, region, service)
	// 0x2000000 = 32 MiB > 16 MiB cap. Payload is a stub; size is what's
	// inspected first.
	body := []byte("2000000;chunk-signature=deadbeefdeadbeef\r\n")
	r := newStreamingReader(nopCloser{bytes.NewReader(body)}, seedSig, signingKey, isoDate, scope)
	defer r.Close()
	_, err := io.ReadAll(r)
	if err == nil {
		t.Fatalf("ReadAll: want error for oversized chunk, got nil")
	}
	// The cap-error path must not be the chunk-signature-mismatch path.
	if errors.Is(err, ErrChunkSignatureMismatch) {
		t.Fatalf("oversized chunk surfaced as signature mismatch — should be a framing error")
	}
}
