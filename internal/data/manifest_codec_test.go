package data

import (
	"reflect"
	"testing"
)

func sampleManifest() *Manifest {
	return &Manifest{
		Class:     "STANDARD",
		Size:      1<<22 + 17,
		ChunkSize: 4 * 1024 * 1024,
		ETag:      `"abc123"`,
		Chunks: []ChunkRef{
			{Cluster: "default", Pool: "rgw.buckets.data", Namespace: "", OID: "k/0", Size: 4 * 1024 * 1024},
			{Cluster: "default", Pool: "rgw.buckets.data", Namespace: "ns", OID: "k/1", Size: 17},
		},
		PartChunks: []int{2, 1, 3},
		PartChecksums: []map[string]string{
			{"x-amz-checksum-crc32": "AAAAAA=="},
			{},
			{"x-amz-checksum-sha256": "abc"},
		},
	}
}

func TestDecodeManifestEmpty(t *testing.T) {
	got, err := DecodeManifest(nil)
	if err != nil || got != nil {
		t.Fatalf("nil input: got %v err=%v", got, err)
	}
	got, err = DecodeManifest([]byte{})
	if err != nil || got != nil {
		t.Fatalf("zero-length input: got %v err=%v", got, err)
	}
}

func TestManifestRoundTripJSON(t *testing.T) {
	m := sampleManifest()
	b, err := EncodeManifestJSON(m)
	if err != nil {
		t.Fatalf("encode json: %v", err)
	}
	if len(b) == 0 || b[0] != '{' {
		t.Fatalf("expected first byte '{', got %q", b[:1])
	}
	got, err := DecodeManifest(b)
	if err != nil {
		t.Fatalf("decode json: %v", err)
	}
	if !reflect.DeepEqual(got, m) {
		t.Fatalf("json round-trip mismatch:\nwant %+v\n got %+v", m, got)
	}
}

func TestManifestRoundTripProto(t *testing.T) {
	m := sampleManifest()
	b, err := EncodeManifestProto(m)
	if err != nil {
		t.Fatalf("encode proto: %v", err)
	}
	if len(b) == 0 {
		t.Fatalf("empty proto output")
	}
	if b[0] == '{' {
		t.Fatalf("proto first byte must not be '{', got %q", b[:1])
	}
	got, err := DecodeManifest(b)
	if err != nil {
		t.Fatalf("decode proto: %v", err)
	}
	// Proto decode normalises empty maps to nil — sampleManifest has one
	// empty map at index 1; expected to come back as nil.
	want := sampleManifest()
	want.PartChecksums[1] = nil
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("proto round-trip mismatch:\nwant %+v\n got %+v", want, got)
	}
}

func TestDecodeManifestReadsBoth(t *testing.T) {
	m := sampleManifest()
	js, err := EncodeManifestJSON(m)
	if err != nil {
		t.Fatalf("encode json: %v", err)
	}
	pb, err := EncodeManifestProto(m)
	if err != nil {
		t.Fatalf("encode proto: %v", err)
	}
	jOut, err := DecodeManifest(js)
	if err != nil {
		t.Fatalf("decode json: %v", err)
	}
	pOut, err := DecodeManifest(pb)
	if err != nil {
		t.Fatalf("decode proto: %v", err)
	}
	if jOut.Class != pOut.Class || jOut.Size != pOut.Size || jOut.ChunkSize != pOut.ChunkSize {
		t.Fatalf("scalar mismatch json=%+v proto=%+v", jOut, pOut)
	}
	if !reflect.DeepEqual(jOut.Chunks, pOut.Chunks) {
		t.Fatalf("chunks mismatch json=%v proto=%v", jOut.Chunks, pOut.Chunks)
	}
	if !reflect.DeepEqual(jOut.PartChunks, pOut.PartChunks) {
		t.Fatalf("part_chunks mismatch json=%v proto=%v", jOut.PartChunks, pOut.PartChunks)
	}
}

func TestDecodeManifestNilSafe(t *testing.T) {
	out, err := EncodeManifestJSON(nil)
	if err != nil || out != nil {
		t.Fatalf("nil json encode: out=%v err=%v", out, err)
	}
	out, err = EncodeManifestProto(nil)
	if err != nil || out != nil {
		t.Fatalf("nil proto encode: out=%v err=%v", out, err)
	}
}

func TestIsJSONManifest(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want bool
	}{
		{"empty", nil, false},
		{"brace", []byte("{}"), true},
		{"leading-ws-brace", []byte("  \n\t{"), true},
		{"proto-tag-1-len", []byte{0x0a, 0x03, 'a', 'b', 'c'}, false},
		{"proto-tag-2-varint", []byte{0x10, 0x05}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isJSONManifest(tc.in); got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

// Verify that ErrEmptyManifest is exported but not used by current API
// (reserved for future encoder-strict callers).
func TestErrEmptyManifestExported(t *testing.T) {
	if ErrEmptyManifest == nil {
		t.Fatalf("ErrEmptyManifest must be non-nil")
	}
}
