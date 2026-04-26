package sse

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"testing"
)

func mustKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, KeySize)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return key
}

func TestEncryptDecryptChunkRoundTrip(t *testing.T) {
	dek := mustKey(t)
	plain := []byte("hello strata")
	ct, err := EncryptChunk(dek, "oid-1", 7, plain)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if bytes.Equal(ct, plain) {
		t.Fatalf("ciphertext equals plaintext")
	}
	got, err := DecryptChunk(dek, "oid-1", 7, ct)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("plaintext mismatch: got %q want %q", got, plain)
	}
}

func TestEncryptChunkEmptyPlaintext(t *testing.T) {
	dek := mustKey(t)
	ct, err := EncryptChunk(dek, "oid", 0, nil)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if len(ct) == 0 {
		t.Fatalf("expected non-empty AEAD output (tag) for empty plaintext")
	}
	pt, err := DecryptChunk(dek, "oid", 0, ct)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if len(pt) != 0 {
		t.Fatalf("expected empty plaintext, got %d bytes", len(pt))
	}
}

func TestDecryptChunkRejectsMutatedCiphertext(t *testing.T) {
	dek := mustKey(t)
	ct, err := EncryptChunk(dek, "oid", 1, []byte("payload"))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	mut := append([]byte(nil), ct...)
	mut[0] ^= 0x01
	if _, err := DecryptChunk(dek, "oid", 1, mut); err == nil {
		t.Fatal("decrypt accepted mutated ciphertext")
	}
}

func TestDecryptChunkRejectsTamperedTag(t *testing.T) {
	dek := mustKey(t)
	ct, err := EncryptChunk(dek, "oid", 1, []byte("payload"))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	mut := append([]byte(nil), ct...)
	mut[len(mut)-1] ^= 0x80
	if _, err := DecryptChunk(dek, "oid", 1, mut); err == nil {
		t.Fatal("decrypt accepted tampered tag")
	}
}

func TestDecryptChunkRejectsMismatchedOID(t *testing.T) {
	dek := mustKey(t)
	ct, err := EncryptChunk(dek, "oid-A", 4, []byte("payload"))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if _, err := DecryptChunk(dek, "oid-B", 4, ct); err == nil {
		t.Fatal("decrypt accepted ciphertext with wrong oid (mismatched IV)")
	}
}

func TestDecryptChunkRejectsMismatchedChunkIndex(t *testing.T) {
	dek := mustKey(t)
	ct, err := EncryptChunk(dek, "oid", 4, []byte("payload"))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if _, err := DecryptChunk(dek, "oid", 5, ct); err == nil {
		t.Fatal("decrypt accepted ciphertext with wrong chunkIndex (mismatched IV)")
	}
}

func TestDecryptChunkRejectsWrongDEK(t *testing.T) {
	dek := mustKey(t)
	other := mustKey(t)
	ct, err := EncryptChunk(dek, "oid", 0, []byte("payload"))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if _, err := DecryptChunk(other, "oid", 0, ct); err == nil {
		t.Fatal("decrypt accepted wrong DEK")
	}
}

func TestEncryptChunkRejectsBadKeyLength(t *testing.T) {
	if _, err := EncryptChunk(make([]byte, 16), "oid", 0, []byte("x")); !errors.Is(err, ErrInvalidKeySize) {
		t.Fatalf("expected ErrInvalidKeySize, got %v", err)
	}
	if _, err := DecryptChunk(make([]byte, 31), "oid", 0, []byte("x")); !errors.Is(err, ErrInvalidKeySize) {
		t.Fatalf("expected ErrInvalidKeySize on Decrypt, got %v", err)
	}
}

func TestDeriveChunkIVDeterministic(t *testing.T) {
	a, err := DeriveChunkIV("oid", 7)
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	b, err := DeriveChunkIV("oid", 7)
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	if !bytes.Equal(a, b) {
		t.Fatalf("HKDF IV not deterministic: %x vs %x", a, b)
	}
	if len(a) != IVSize {
		t.Fatalf("expected %d-byte IV, got %d", IVSize, len(a))
	}
}

func TestDeriveChunkIVDiffersByOID(t *testing.T) {
	a, _ := DeriveChunkIV("oid-A", 0)
	b, _ := DeriveChunkIV("oid-B", 0)
	if bytes.Equal(a, b) {
		t.Fatalf("IV collision across oids: %x", a)
	}
}

func TestDeriveChunkIVDiffersByIndex(t *testing.T) {
	a, _ := DeriveChunkIV("oid", 0)
	b, _ := DeriveChunkIV("oid", 1)
	if bytes.Equal(a, b) {
		t.Fatalf("IV collision across chunk indexes: %x", a)
	}
}

func TestEncryptChunkSameInputsProduceSameCiphertext(t *testing.T) {
	dek := mustKey(t)
	a, err := EncryptChunk(dek, "oid", 0, []byte("payload"))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	b, err := EncryptChunk(dek, "oid", 0, []byte("payload"))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if !bytes.Equal(a, b) {
		t.Fatalf("deterministic IV should yield same ciphertext for same inputs")
	}
}

func TestWrapUnwrapDEKRoundTrip(t *testing.T) {
	master := mustKey(t)
	dek := mustKey(t)
	wrapped, err := WrapDEK(master, dek)
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}
	if bytes.Contains(wrapped, dek) {
		t.Fatal("wrapped blob leaks plaintext DEK")
	}
	got, err := UnwrapDEK(master, wrapped)
	if err != nil {
		t.Fatalf("unwrap: %v", err)
	}
	if !bytes.Equal(got, dek) {
		t.Fatalf("DEK mismatch after wrap/unwrap")
	}
}

func TestWrapDEKUsesRandomNonce(t *testing.T) {
	master := mustKey(t)
	dek := mustKey(t)
	a, err := WrapDEK(master, dek)
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}
	b, err := WrapDEK(master, dek)
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}
	if bytes.Equal(a, b) {
		t.Fatalf("expected distinct wrapped blobs (random nonce)")
	}
}

func TestUnwrapDEKRejectsWrongMaster(t *testing.T) {
	master := mustKey(t)
	other := mustKey(t)
	dek := mustKey(t)
	wrapped, err := WrapDEK(master, dek)
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}
	if _, err := UnwrapDEK(other, wrapped); err == nil {
		t.Fatal("unwrap accepted wrong master key")
	}
}

func TestUnwrapDEKRejectsTruncated(t *testing.T) {
	master := mustKey(t)
	if _, err := UnwrapDEK(master, []byte{1, 2, 3}); !errors.Is(err, ErrShortWrappedDEK) {
		t.Fatalf("expected ErrShortWrappedDEK, got %v", err)
	}
}

func TestUnwrapDEKRejectsTampered(t *testing.T) {
	master := mustKey(t)
	dek := mustKey(t)
	wrapped, err := WrapDEK(master, dek)
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}
	mut := append([]byte(nil), wrapped...)
	mut[len(mut)-1] ^= 0x01
	if _, err := UnwrapDEK(master, mut); err == nil {
		t.Fatal("unwrap accepted tampered ciphertext")
	}
}

func TestWrapDEKRejectsBadKeySizes(t *testing.T) {
	if _, err := WrapDEK(make([]byte, 16), make([]byte, KeySize)); !errors.Is(err, ErrInvalidKeySize) {
		t.Fatalf("expected ErrInvalidKeySize for short master, got %v", err)
	}
	if _, err := WrapDEK(make([]byte, KeySize), make([]byte, 16)); !errors.Is(err, ErrInvalidKeySize) {
		t.Fatalf("expected ErrInvalidKeySize for short dek, got %v", err)
	}
}
