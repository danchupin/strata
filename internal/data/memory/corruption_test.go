package memory_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/danchupin/strata/internal/data"
	datamem "github.com/danchupin/strata/internal/data/memory"
)

// errMidStream is the sentinel an upload reader returns after delivering some
// bytes, standing in for a client connection that drops mid-PUT.
var errMidStream = errors.New("simulated mid-stream upload failure")

// errAfterReader yields all of data, then returns errMidStream forever. It
// drives PutChunks through several full chunks before the failure so the
// mid-stream (not first-byte) path is exercised.
type errAfterReader struct {
	data []byte
	pos  int
}

func (r *errAfterReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, errMidStream
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

// TestPutChunksPartialWriteCommitsNoManifest pins the US-011 partial-write
// rollback invariant for the default-tag (memory) data plane: a PutChunks
// whose source reader fails mid-stream returns the error and a nil manifest,
// so the gateway — which persists the object meta row ONLY after PutChunks
// returns a non-nil manifest with a nil error — never commits a readable
// half-object. Any chunks already buffered are orphaned bytes (a GC concern),
// never a reachable object.
func TestPutChunksPartialWriteCommitsNoManifest(t *testing.T) {
	b := datamem.New()
	t.Cleanup(func() { _ = b.Close() })

	// 9 MiB forces three 4 MiB chunk iterations: two complete, the third
	// reads ~1 MiB then hits errMidStream — proving the failure is handled
	// after partial progress, not only on an empty reader.
	payload := bytes.Repeat([]byte("x"), 9*1024*1024)
	r := &errAfterReader{data: payload}

	m, err := b.PutChunks(context.Background(), r, "STANDARD")
	if !errors.Is(err, errMidStream) {
		t.Fatalf("PutChunks: want errMidStream, got %v", err)
	}
	if m != nil {
		t.Fatalf("PutChunks returned a non-nil manifest on mid-stream failure: %+v", m)
	}
}

// TestPutChunksContextCancelCommitsNoManifest is the cancellation twin: a PUT
// whose context is cancelled before completion must likewise return an error
// and no manifest. PutChunks checks ctx.Err() at the top of every chunk
// iteration, so a pre-cancelled context never yields a committed object.
func TestPutChunksContextCancelCommitsNoManifest(t *testing.T) {
	b := datamem.New()
	t.Cleanup(func() { _ = b.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	m, err := b.PutChunks(ctx, bytes.NewReader(bytes.Repeat([]byte("y"), 8*1024*1024)), "STANDARD")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("PutChunks: want context.Canceled, got %v", err)
	}
	if m != nil {
		t.Fatalf("PutChunks returned a non-nil manifest on cancelled context: %+v", m)
	}
}

// TestGetChunksZeroByteManifestReadsClean is a control: a legitimate empty
// object round-trips with a clean EOF, so the fail-loud assertions above are
// proving real damage detection, not a backend that errors on everything.
func TestGetChunksZeroByteManifestReadsClean(t *testing.T) {
	b := datamem.New()
	t.Cleanup(func() { _ = b.Close() })

	m, err := b.PutChunks(context.Background(), bytes.NewReader(nil), "STANDARD")
	if err != nil {
		t.Fatalf("PutChunks(empty): %v", err)
	}
	if m == nil {
		t.Fatal("PutChunks(empty) returned nil manifest")
	}
	rc, err := b.GetChunks(context.Background(), m, 0, m.Size)
	if err != nil {
		t.Fatalf("GetChunks(empty): %v", err)
	}
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatalf("read empty object: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("empty object read %d bytes, want 0", len(got))
	}
	// Guard against an accidental import-only reference to data.
	_ = data.DefaultChunkSize
}
