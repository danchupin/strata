package auth

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

// Streaming SigV4 chunk-signature test vectors.
//
// AWS publishes the chained-HMAC formula at
// https://docs.aws.amazon.com/AmazonS3/latest/API/sigv4-streaming.html
// — sig(chunk_n) = HMAC(signingKey,
//      "AWS4-HMAC-SHA256-PAYLOAD\n<reqDate>\n<scope>\n<prevSig>\n<hash("")>\n<hash(chunk)>")
// The tests below exercise the formula end-to-end (positive round-trip and
// the rejection paths) using a deterministic seed signature.
const (
	streamSecret    = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
	streamDate      = "20130524"
	streamRegion    = "us-east-1"
	streamService   = "s3"
	streamTimestamp = "20130524T000000Z"
	streamSeedSig   = "4f232c4386841ef735655705268965c44a0e4690baa4adea153f7db9fa80a0a9"
)

func streamSigningKey() []byte {
	return deriveSigningKey(streamSecret, streamDate, streamRegion, streamService)
}

func streamScope() string {
	return credentialScope(streamDate, streamRegion, streamService)
}

func encodeChunk(data []byte, sig string) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "%x;chunk-signature=%s\r\n", len(data), sig)
	b.Write(data)
	if len(data) > 0 {
		b.WriteString("\r\n")
	}
	return b.Bytes()
}

// awsStreamingBody constructs an aws-chunked body for the given chunks
// using the streaming-SigV4 chained-HMAC formula. Returns the encoded body
// and the concatenated plaintext.
func awsStreamingBody(chunks [][]byte) (encoded []byte, plain []byte, finalSig string) {
	key := streamSigningKey()
	scope := streamScope()
	prev := streamSeedSig
	for _, c := range chunks {
		sig := computeChunkSignature(key, streamTimestamp, scope, prev, c)
		encoded = append(encoded, encodeChunk(c, sig)...)
		plain = append(plain, c...)
		prev = sig
	}
	finalSig = computeChunkSignature(key, streamTimestamp, scope, prev, nil)
	encoded = append(encoded, encodeChunk(nil, finalSig)...)
	return encoded, plain, finalSig
}

func TestStreamingReaderPositive(t *testing.T) {
	body, plain, _ := awsStreamingBody([][]byte{
		bytes.Repeat([]byte{'a'}, 65536),
		bytes.Repeat([]byte{'a'}, 1024),
	})
	r := newStreamingReader(io.NopCloser(bytes.NewReader(body)), streamSigningKey(), streamTimestamp, streamScope(), streamSeedSig)
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("decoded body mismatch: got %d bytes, want %d", len(got), len(plain))
	}
}

func TestStreamingReaderChunkChainBindsToPrev(t *testing.T) {
	// If chunk2 had been signed against the wrong prevSig (e.g. seed instead
	// of sig1), the verifier must reject — proves the chain is honored.
	chunk1 := bytes.Repeat([]byte{'a'}, 16)
	chunk2 := bytes.Repeat([]byte{'b'}, 16)
	key := streamSigningKey()
	scope := streamScope()
	sig1 := computeChunkSignature(key, streamTimestamp, scope, streamSeedSig, chunk1)
	wrongSig2 := computeChunkSignature(key, streamTimestamp, scope, streamSeedSig, chunk2) // chained to seed, not to sig1
	finalSig := computeChunkSignature(key, streamTimestamp, scope, wrongSig2, nil)
	body := append([]byte{}, encodeChunk(chunk1, sig1)...)
	body = append(body, encodeChunk(chunk2, wrongSig2)...)
	body = append(body, encodeChunk(nil, finalSig)...)

	r := newStreamingReader(io.NopCloser(bytes.NewReader(body)), key, streamTimestamp, streamScope(), streamSeedSig)
	_, err := io.ReadAll(r)
	if !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("expected ErrSignatureInvalid on broken chain, got %v", err)
	}
}

func TestStreamingReaderMutatedChunkRejected(t *testing.T) {
	body, _, _ := awsStreamingBody([][]byte{
		bytes.Repeat([]byte{'a'}, 64),
	})
	// Flip a byte inside the data section.
	idx := bytes.Index(body, []byte("\r\n")) + 2 // first CRLF closes header
	body[idx] ^= 0xFF

	r := newStreamingReader(io.NopCloser(bytes.NewReader(body)), streamSigningKey(), streamTimestamp, streamScope(), streamSeedSig)
	_, err := io.ReadAll(r)
	if !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("expected ErrSignatureInvalid, got %v", err)
	}
}

func TestStreamingReaderMissingFinalChunk(t *testing.T) {
	key := streamSigningKey()
	scope := streamScope()
	chunk1 := bytes.Repeat([]byte{'a'}, 64)
	sig1 := computeChunkSignature(key, streamTimestamp, scope, streamSeedSig, chunk1)
	body := encodeChunk(chunk1, sig1) // no terminator chunk

	r := newStreamingReader(io.NopCloser(bytes.NewReader(body)), key, streamTimestamp, streamScope(), streamSeedSig)
	_, err := io.ReadAll(r)
	if !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("expected ErrSignatureInvalid on missing terminator, got %v", err)
	}
}

func TestStreamingReaderMissingChunkSignatureRejected(t *testing.T) {
	chunk1 := bytes.Repeat([]byte{'a'}, 16)
	header := fmt.Sprintf("%x\r\n", len(chunk1)) // no chunk-signature
	body := append([]byte(header), chunk1...)
	body = append(body, []byte("\r\n")...)
	r := newStreamingReader(io.NopCloser(bytes.NewReader(body)), streamSigningKey(), streamTimestamp, streamScope(), streamSeedSig)
	_, err := io.ReadAll(r)
	if !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("expected ErrSignatureInvalid on missing chunk-signature, got %v", err)
	}
}

func TestStreamingReaderTamperedFinalChunkRejected(t *testing.T) {
	body, _, finalSig := awsStreamingBody([][]byte{
		bytes.Repeat([]byte{'a'}, 32),
	})
	tampered := strings.Replace(string(body), finalSig, strings.Repeat("0", 64), 1)
	r := newStreamingReader(io.NopCloser(bytes.NewReader([]byte(tampered))), streamSigningKey(), streamTimestamp, streamScope(), streamSeedSig)
	_, err := io.ReadAll(r)
	if !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("expected ErrSignatureInvalid on tampered final chunk, got %v", err)
	}
}
