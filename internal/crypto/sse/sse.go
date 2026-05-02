// Package sse implements AES-256-GCM AEAD primitives used by SSE-S3:
//
//   - EncryptChunk / DecryptChunk seal a single object chunk under a per-object
//     32-byte DEK with a deterministic 12-byte IV derived via HKDF-SHA256 from
//     the object id and chunk index. The IV is recomputed on decrypt, so it is
//     never persisted.
//   - WrapDEK / UnwrapDEK wrap the per-object DEK under the cluster master key.
//     A random 12-byte nonce is prefixed to the ciphertext+tag and returned to
//     the caller as a single byte slice for storage.
package sse

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/hkdf"
)

const (
	// KeySize is the required DEK / master-key length (AES-256).
	KeySize = 32
	// IVSize is the AES-GCM nonce length.
	IVSize = 12

	hkdfInfoChunkIV = "strata-sse-chunk-iv"
)

// ErrInvalidKeySize is returned when a key argument is not exactly KeySize bytes.
var ErrInvalidKeySize = errors.New("strata/crypto/sse: key must be 32 bytes")

// ErrShortWrappedDEK is returned when UnwrapDEK gets fewer bytes than IVSize+tag.
var ErrShortWrappedDEK = errors.New("strata/crypto/sse: wrapped DEK truncated")

// EncryptChunk seals plaintext under dek using AES-256-GCM with an IV derived
// from (oid, chunkIndex). The returned slice is ciphertext||tag (no nonce
// prefix — the receiver recomputes the IV).
func EncryptChunk(dek []byte, oid string, chunkIndex uint64, plaintext []byte) ([]byte, error) {
	gcm, err := newGCM(dek)
	if err != nil {
		return nil, err
	}
	iv, err := DeriveChunkIV(oid, chunkIndex)
	if err != nil {
		return nil, err
	}
	return gcm.Seal(nil, iv, plaintext, nil), nil
}

// DecryptChunk reverses EncryptChunk. AEAD failures (mutated ciphertext or
// mismatched oid/chunkIndex) surface as a non-nil error.
func DecryptChunk(dek []byte, oid string, chunkIndex uint64, ciphertext []byte) ([]byte, error) {
	gcm, err := newGCM(dek)
	if err != nil {
		return nil, err
	}
	iv, err := DeriveChunkIV(oid, chunkIndex)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, iv, ciphertext, nil)
}

// WrapDEK encrypts dek under masterKey via AES-256-GCM with a random nonce.
// The returned blob is nonce||ciphertext||tag and is safe to persist verbatim.
func WrapDEK(masterKey, dek []byte) ([]byte, error) {
	if len(dek) != KeySize {
		return nil, fmt.Errorf("%w: dek %d bytes", ErrInvalidKeySize, len(dek))
	}
	gcm, err := newGCM(masterKey)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, IVSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("strata/crypto/sse: rand: %w", err)
	}
	out := make([]byte, IVSize, IVSize+len(dek)+gcm.Overhead())
	copy(out, nonce)
	return gcm.Seal(out, nonce, dek, nil), nil
}

// UnwrapDEK reverses WrapDEK. Returns ErrShortWrappedDEK if the blob is too
// short to contain a nonce + tag; AEAD failures bubble up as the underlying
// cipher error.
func UnwrapDEK(masterKey, wrapped []byte) ([]byte, error) {
	gcm, err := newGCM(masterKey)
	if err != nil {
		return nil, err
	}
	if len(wrapped) < IVSize+gcm.Overhead() {
		return nil, ErrShortWrappedDEK
	}
	nonce := wrapped[:IVSize]
	ct := wrapped[IVSize:]
	return gcm.Open(nil, nonce, ct, nil)
}

// DeriveChunkIV produces a deterministic 12-byte AES-GCM nonce from the object
// id and chunk index via HKDF-SHA256. Exposed for tests and callers that need
// to verify IV uniqueness.
func DeriveChunkIV(oid string, chunkIndex uint64) ([]byte, error) {
	var idx [8]byte
	binary.BigEndian.PutUint64(idx[:], chunkIndex)
	ikm := make([]byte, 0, len(oid)+8)
	ikm = append(ikm, oid...)
	ikm = append(ikm, idx[:]...)
	r := hkdf.New(sha256.New, ikm, nil, []byte(hkdfInfoChunkIV))
	iv := make([]byte, IVSize)
	if _, err := io.ReadFull(r, iv); err != nil {
		return nil, fmt.Errorf("strata/crypto/sse: hkdf: %w", err)
	}
	return iv, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("%w: got %d bytes", ErrInvalidKeySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("strata/crypto/sse: aes: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("strata/crypto/sse: gcm: %w", err)
	}
	return gcm, nil
}
