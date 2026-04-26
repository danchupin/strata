package s3api

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"

	"github.com/danchupin/strata/internal/crypto/sse"
	"github.com/danchupin/strata/internal/data"
)

// sseTagSize is the AES-GCM tag length appended to each chunk's ciphertext.
const sseTagSize = 16

// ssePlaintextChunkSize is the largest plaintext chunk we hand to EncryptChunk.
// Sized so that the resulting ciphertext (plaintext + tag) lands in exactly one
// data-backend chunk (data.DefaultChunkSize). This keeps crypto chunk index
// equal to backend chunk index for deterministic decrypt-on-read.
const ssePlaintextChunkSize = data.DefaultChunkSize - sseTagSize

// sseEncryptingReader streams plaintext from a source reader and emits
// ciphertext shaped as concatenated AES-GCM chunks. Each plaintext window of
// up to ssePlaintextChunkSize bytes becomes (window || tag). Aligning the
// crypto chunk size to data.DefaultChunkSize-sseTagSize means every full
// ciphertext chunk is exactly data.DefaultChunkSize bytes, so the data
// backend's 4 MiB chunker produces one backend chunk per crypto chunk.
type sseEncryptingReader struct {
	src        io.Reader
	dek        []byte
	oid        string
	chunkIndex uint64
	pSize      int
	pBuf       []byte
	cur        []byte
	curOff     int
	srcMD5     hash.Hash
	srcSize    int64
	srcErr     error
}

func newSSEEncryptingReader(src io.Reader, dek []byte, oid string) *sseEncryptingReader {
	return &sseEncryptingReader{
		src:    src,
		dek:    dek,
		oid:    oid,
		pSize:  int(ssePlaintextChunkSize),
		pBuf:   make([]byte, ssePlaintextChunkSize),
		srcMD5: md5.New(),
	}
}

func (r *sseEncryptingReader) Read(p []byte) (int, error) {
	if r.curOff >= len(r.cur) {
		if err := r.fillNext(); err != nil {
			return 0, err
		}
	}
	n := copy(p, r.cur[r.curOff:])
	r.curOff += n
	return n, nil
}

func (r *sseEncryptingReader) fillNext() error {
	if r.srcErr != nil {
		return r.srcErr
	}
	n, err := io.ReadFull(r.src, r.pBuf)
	if n > 0 {
		r.srcMD5.Write(r.pBuf[:n])
		r.srcSize += int64(n)
		ct, eerr := sse.EncryptChunk(r.dek, r.oid, r.chunkIndex, r.pBuf[:n])
		if eerr != nil {
			r.srcErr = eerr
			return eerr
		}
		r.cur = ct
		r.curOff = 0
		r.chunkIndex++
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		r.srcErr = io.EOF
		if n == 0 {
			return io.EOF
		}
		return nil
	}
	if err != nil {
		r.srcErr = err
		if n == 0 {
			return err
		}
	}
	return nil
}

// PlaintextSize returns the total number of plaintext bytes consumed from src.
// Valid only after the reader has been fully drained.
func (r *sseEncryptingReader) PlaintextSize() int64 { return r.srcSize }

// PlaintextETag returns the hex-encoded MD5 of the plaintext bytes consumed.
// Valid only after the reader has been fully drained.
func (r *sseEncryptingReader) PlaintextETag() string {
	return hex.EncodeToString(r.srcMD5.Sum(nil))
}

// sseChunkLocator returns, for a given flat chunk index, the (oid, chunkIndex)
// pair that was used during encryption. For single-PUT objects this is just
// (key, idx). For multipart objects the OID and per-part chunk index vary by
// part — see chunkLocatorFromManifest.
type sseChunkLocator func(flatIdx int) (oid string, chunkIndexInPart uint64)

// sseDecryptingReader streams plaintext from an SSE-encrypted manifest. Range
// requests are translated from plaintext space to ciphertext space chunk by
// chunk: each crypto chunk maps 1:1 to a backend chunk in the manifest, so
// chunk i lives at byte offset sum(m.Chunks[0..i-1].Size) in ciphertext space.
type sseDecryptingReader struct {
	ctx     context.Context
	backend data.Backend
	m       *data.Manifest
	dek     []byte
	locator sseChunkLocator

	pStart int64 // plaintext start offset (inclusive)
	pEnd   int64 // plaintext end offset (exclusive)
	pPos   int64 // next plaintext byte to deliver

	curIdx int
	cur    []byte
	curOff int

	chunkOffsets []int64 // ciphertext byte offset where chunk i begins
	chunkPSizes  []int64 // plaintext size of chunk i (chunk.Size - sseTagSize)
}

func newSSEDecryptingReader(ctx context.Context, backend data.Backend, m *data.Manifest, dek []byte, oid string, pOffset, pLength int64) *sseDecryptingReader {
	return newSSEDecryptingReaderWithLocator(ctx, backend, m, dek, singleChunkLocator(oid), pOffset, pLength)
}

func newSSEDecryptingReaderWithLocator(ctx context.Context, backend data.Backend, m *data.Manifest, dek []byte, locator sseChunkLocator, pOffset, pLength int64) *sseDecryptingReader {
	offsets := make([]int64, len(m.Chunks))
	psizes := make([]int64, len(m.Chunks))
	var off int64
	for i, c := range m.Chunks {
		offsets[i] = off
		off += c.Size
		psizes[i] = c.Size - int64(sseTagSize)
	}
	return &sseDecryptingReader{
		ctx:          ctx,
		backend:      backend,
		m:            m,
		dek:          dek,
		locator:      locator,
		pStart:       pOffset,
		pEnd:         pOffset + pLength,
		pPos:         pOffset,
		curIdx:       -1,
		chunkOffsets: offsets,
		chunkPSizes:  psizes,
	}
}

// singleChunkLocator returns a locator that produces (oid, idx) for every
// chunk — i.e. the single-PUT object layout where every chunk uses the same
// OID and its flat manifest index as the chunk-index input to the IV HKDF.
func singleChunkLocator(oid string) sseChunkLocator {
	return func(idx int) (string, uint64) { return oid, uint64(idx) }
}

// multipartChunkLocator builds a locator over a manifest's PartChunks list.
// For chunk i in the merged manifest, it finds which part the chunk came from
// (and that part's local chunk index) so the IV input matches what was used
// during UploadPart, where oid = key + ":part=" + partNumber and the
// chunkIndex restarts at 0 inside each part.
func multipartChunkLocator(key string, partChunks []int) sseChunkLocator {
	return func(idx int) (string, uint64) {
		base := 0
		for partOffset, count := range partChunks {
			if idx < base+count {
				return multipartPartOID(key, partOffset+1), uint64(idx - base)
			}
			base += count
		}
		return key, uint64(idx)
	}
}

func multipartPartOID(key string, partNumber int) string {
	return fmt.Sprintf("%s:part=%d", key, partNumber)
}

// Preload eagerly loads and decrypts the first chunk in the requested range
// so that AEAD failures surface as a Read error before the caller commits a
// response status. Safe to call multiple times — subsequent calls are no-ops.
func (r *sseDecryptingReader) Preload() error {
	if r.cur != nil || r.pPos >= r.pEnd {
		return nil
	}
	return r.loadChunkAt(r.pPos)
}

func (r *sseDecryptingReader) Read(p []byte) (int, error) {
	if r.pPos >= r.pEnd {
		return 0, io.EOF
	}
	if r.curOff >= len(r.cur) {
		if err := r.loadChunkAt(r.pPos); err != nil {
			return 0, err
		}
	}
	avail := len(r.cur) - r.curOff
	remain := int(r.pEnd - r.pPos)
	want := len(p)
	if want > avail {
		want = avail
	}
	if want > remain {
		want = remain
	}
	n := copy(p[:want], r.cur[r.curOff:r.curOff+want])
	r.curOff += n
	r.pPos += int64(n)
	return n, nil
}

func (r *sseDecryptingReader) loadChunkAt(pPos int64) error {
	idx, startInChunk, err := r.locateChunk(pPos)
	if err != nil {
		return err
	}
	cOff := r.chunkOffsets[idx]
	cSize := r.m.Chunks[idx].Size
	rc, err := r.backend.GetChunks(r.ctx, r.m, cOff, cSize)
	if err != nil {
		return fmt.Errorf("sse: fetch ciphertext chunk %d: %w", idx, err)
	}
	defer rc.Close()
	ct, err := io.ReadAll(rc)
	if err != nil {
		return fmt.Errorf("sse: read ciphertext chunk %d: %w", idx, err)
	}
	oid, chunkIndex := r.locator(idx)
	pt, err := sse.DecryptChunk(r.dek, oid, chunkIndex, ct)
	if err != nil {
		return fmt.Errorf("sse: decrypt chunk %d: %w", idx, err)
	}
	r.cur = pt
	r.curOff = startInChunk
	r.curIdx = idx
	return nil
}

// locateChunk returns the chunk index and within-chunk plaintext offset for
// the given absolute plaintext position by walking chunkPSizes — handles the
// (rare) case where chunk plaintext sizes differ from ssePlaintextChunkSize,
// e.g. the last chunk.
func (r *sseDecryptingReader) locateChunk(pPos int64) (idx int, startInChunk int, err error) {
	var base int64
	for i, ps := range r.chunkPSizes {
		if pPos < base+ps {
			return i, int(pPos - base), nil
		}
		base += ps
	}
	return 0, 0, io.EOF
}

func (r *sseDecryptingReader) Close() error { return nil }
