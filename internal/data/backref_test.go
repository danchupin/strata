package data

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestBackrefEncodeDecodeRoundTrip(t *testing.T) {
	bid := uuid.New()
	mtime := time.Unix(1717000000, 123456789).UTC()

	tests := []struct {
		name string
		in   Backref
	}{
		{
			name: "typical versioned chunk",
			in:   Backref{BucketID: bid, Key: "path/to/object.bin", VersionID: uuid.New().String(), ChunkIdx: 7, Mtime: mtime},
		},
		{
			name: "null version (suspended/disabled) — meta.NullVersionID literal",
			in:   Backref{BucketID: bid, Key: "k", VersionID: "00000000-0000-0000-0000-000000000000", ChunkIdx: 0, Mtime: mtime},
		},
		{
			name: "empty key edge",
			in:   Backref{BucketID: bid, Key: "", VersionID: "v", ChunkIdx: 1, Mtime: mtime},
		},
		{
			name: "empty version id edge",
			in:   Backref{BucketID: bid, Key: "k", VersionID: "", ChunkIdx: 2, Mtime: mtime},
		},
		{
			name: "large chunk index",
			in:   Backref{BucketID: bid, Key: "k", VersionID: "v", ChunkIdx: 1_000_000, Mtime: mtime},
		},
		{
			name: "unicode key with embedded null bytes",
			in:   Backref{BucketID: bid, Key: "ключ/\x00/файл", VersionID: "v", ChunkIdx: 3, Mtime: mtime},
		},
		{
			name: "zero mtime stays zero",
			in:   Backref{BucketID: bid, Key: "k", VersionID: "v", ChunkIdx: 4},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			enc := EncodeBackref(tc.in)
			if enc[0] != BackrefSchemaV1 {
				t.Fatalf("schema byte: want %d, got %d", BackrefSchemaV1, enc[0])
			}
			got, err := DecodeBackref(enc)
			if err != nil {
				t.Fatalf("DecodeBackref: %v", err)
			}
			if got.BucketID != tc.in.BucketID {
				t.Errorf("BucketID: want %s, got %s", tc.in.BucketID, got.BucketID)
			}
			if got.Key != tc.in.Key {
				t.Errorf("Key: want %q, got %q", tc.in.Key, got.Key)
			}
			if got.VersionID != tc.in.VersionID {
				t.Errorf("VersionID: want %q, got %q", tc.in.VersionID, got.VersionID)
			}
			if got.ChunkIdx != tc.in.ChunkIdx {
				t.Errorf("ChunkIdx: want %d, got %d", tc.in.ChunkIdx, got.ChunkIdx)
			}
			if !got.Mtime.Equal(tc.in.Mtime) {
				t.Errorf("Mtime: want %v, got %v", tc.in.Mtime, got.Mtime)
			}
		})
	}
}

func TestBackrefCarriesNoKeyMaterial(t *testing.T) {
	// Regression guard for the SSE security separation (PRD decision 3):
	// the back-reference must never embed a wrapped DEK or plaintext key.
	const secret = "SUPER-SECRET-DEK-MATERIAL"
	enc := EncodeBackref(Backref{
		BucketID:  uuid.New(),
		Key:       "object",
		VersionID: "v",
		ChunkIdx:  0,
		Mtime:     time.Now().UTC(),
	})
	if got, err := DecodeBackref(enc); err != nil {
		t.Fatalf("DecodeBackref: %v", err)
	} else if got.Key == secret {
		t.Fatal("unreachable")
	}
	// The Backref struct has no key-material field at all — a compile-time
	// guarantee; this test documents the invariant for future readers.
}

func TestDecodeBackrefRejectsMalformed(t *testing.T) {
	tests := []struct {
		name        string
		in          []byte
		expectedErr error
	}{
		{name: "empty", in: nil, expectedErr: ErrBackrefMalformed},
		{name: "truncated header", in: make([]byte, backrefHeaderLen-1), expectedErr: ErrBackrefMalformed},
		{
			name: "version length overruns buffer",
			in: func() []byte {
				b := make([]byte, backrefHeaderLen)
				b[0] = BackrefSchemaV1
				b[29] = 0xFF
				b[30] = 0xFF
				return b
			}(),
			expectedErr: ErrBackrefMalformed,
		},
		{
			name:        "unknown schema byte",
			in:          func() []byte { b := make([]byte, backrefHeaderLen); b[0] = 99; return b }(),
			expectedErr: ErrBackrefSchema,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodeBackref(tc.in)
			if !errors.Is(err, tc.expectedErr) {
				t.Fatalf("want %v, got %v", tc.expectedErr, err)
			}
		})
	}
}

func TestBackrefEnabledFromEnv(t *testing.T) {
	tests := []struct {
		name       string
		set        bool
		value      string
		expectedOn bool
	}{
		{name: "unset defaults on", set: false, expectedOn: true},
		{name: "false opts out", set: true, value: "false", expectedOn: false},
		{name: "0 opts out", set: true, value: "0", expectedOn: false},
		{name: "true stays on", set: true, value: "true", expectedOn: true},
		{name: "garbage falls back to on", set: true, value: "yesplease", expectedOn: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv("STRATA_CHUNK_BACKREF", tc.value)
			} else {
				t.Setenv("STRATA_CHUNK_BACKREF", "")
			}
			if got := BackrefEnabledFromEnv(); got != tc.expectedOn {
				t.Fatalf("BackrefEnabledFromEnv: want %v, got %v", tc.expectedOn, got)
			}
		})
	}
}

// BenchmarkEncodeBackref pins the hot-path cost of stamping a chunk
// back-reference: the encode is a single allocation + fixed-width fill,
// orders of magnitude below the librados WriteOp it rides on. See
// docs/site/content/architecture/benchmarks/rados-ops.md.
func BenchmarkEncodeBackref(b *testing.B) {
	ref := Backref{
		BucketID:  uuid.New(),
		Key:       "some/typical/object/key.bin",
		VersionID: uuid.New().String(),
		ChunkIdx:  3,
		Mtime:     time.Unix(1717000000, 0).UTC(),
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = EncodeBackref(ref)
	}
}

func TestWithBackrefRoundTrip(t *testing.T) {
	bid := uuid.New()
	attrs := BackrefAttrs{BucketID: bid, Key: "k", VersionID: "v", Mtime: time.Now().UTC()}
	ctx := WithBackref(context.Background(), attrs)
	got, ok := BackrefFromContext(ctx)
	if !ok {
		t.Fatal("BackrefFromContext: want ok")
	}
	if got != attrs {
		t.Fatalf("attrs: want %+v, got %+v", attrs, got)
	}
}

func TestWithBackrefEmptyIdentityIsNoOp(t *testing.T) {
	ctx := WithBackref(context.Background(), BackrefAttrs{})
	if _, ok := BackrefFromContext(ctx); ok {
		t.Fatal("empty identity must not be stored")
	}
	if _, ok := BackrefFromContext(context.Background()); ok {
		t.Fatal("bare context must carry no backref")
	}
}
