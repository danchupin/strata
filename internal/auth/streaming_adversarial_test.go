package auth

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"testing"
)

// US-004 — streaming chunk-signature adversarial additions.
//
// The chain-HMAC happy path and several rejection legs already live in
// streaming_test.go (broken chain / mutated chunk / missing-final /
// missing-chunk-sig / tampered-final) and streaming_trailer_test.go
// (trailer-sig + body-checksum mismatch). This file adds the two classes the
// US-004 matrix calls for that those files do NOT cover: an out-of-order chunk
// and a truncated chunk body. Trailer-checksum mismatch is intentionally NOT
// re-asserted here — see TestStreamingTrailerReaderBodyChecksumMismatchRejected
// and TestStreamingTrailerReaderAllAlgos.

// TestStreamingReaderOutOfOrderChunkRejected proves the chain is order-binding:
// two chunks each signed correctly for their position, then emitted in swapped
// order. The first chunk read is chunk2's data carrying chunk2's signature
// (chained to sig1), but the reader's prevSig is still the seed → mismatch.
func TestStreamingReaderOutOfOrderChunkRejected(t *testing.T) {
	key := streamSigningKey()
	scope := streamScope()
	chunk1 := bytes.Repeat([]byte{'a'}, 64)
	chunk2 := bytes.Repeat([]byte{'b'}, 64)

	sig1 := computeChunkSignature(key, streamTimestamp, scope, streamSeedSig, chunk1)
	sig2 := computeChunkSignature(key, streamTimestamp, scope, sig1, chunk2)
	finalSig := computeChunkSignature(key, streamTimestamp, scope, sig2, nil)

	// Emit chunk2 BEFORE chunk1 — the wire order no longer matches the
	// signing order, so the first chunk verified against the seed fails.
	var body []byte
	body = append(body, encodeChunk(chunk2, sig2)...)
	body = append(body, encodeChunk(chunk1, sig1)...)
	body = append(body, encodeChunk(nil, finalSig)...)

	r := newStreamingReader(io.NopCloser(bytes.NewReader(body)), key, streamTimestamp, streamScope(), streamSeedSig)
	if _, err := io.ReadAll(r); !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("expected ErrSignatureInvalid on out-of-order chunk, got %v", err)
	}
}

// TestStreamingReaderTruncatedChunkDataRejected proves a chunk whose declared
// size exceeds the bytes actually present fails loud (io.ErrUnexpectedEOF from
// io.ReadFull) rather than silently decoding a short body. This is distinct
// from TestStreamingReaderMissingFinalChunk (a complete data chunk with no
// terminator → ErrSignatureInvalid): here the data section itself is truncated.
func TestStreamingReaderTruncatedChunkDataRejected(t *testing.T) {
	declared := 64
	// Header declares 64 bytes but only 10 follow before EOF.
	header := fmt.Sprintf("%x;chunk-signature=%s\r\n", declared, "deadbeef")
	body := append([]byte(header), bytes.Repeat([]byte{'a'}, 10)...)

	r := newStreamingReader(io.NopCloser(bytes.NewReader(body)), streamSigningKey(), streamTimestamp, streamScope(), streamSeedSig)
	_, err := io.ReadAll(r)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("expected io.ErrUnexpectedEOF on truncated chunk data, got %v", err)
	}
}
