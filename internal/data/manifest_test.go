package data

import (
	"reflect"
	"strings"
	"testing"
)

func TestEncodeDecodeNilManifest(t *testing.T) {
	b, err := EncodeManifest(nil)
	if err != nil {
		t.Fatalf("encode nil: %v", err)
	}
	if b != nil {
		t.Fatalf("encode nil: want nil bytes, got %q", b)
	}
	m, err := DecodeManifest(nil)
	if err != nil {
		t.Fatalf("decode nil: %v", err)
	}
	if m != nil {
		t.Fatalf("decode nil: want nil manifest, got %+v", m)
	}
	m, err = DecodeManifest([]byte{})
	if err != nil {
		t.Fatalf("decode empty: %v", err)
	}
	if m != nil {
		t.Fatalf("decode empty: want nil manifest, got %+v", m)
	}
}

func TestRoundTripBackendRefVersionIDShapes(t *testing.T) {
	cases := []struct {
		name      string
		versionID string
	}{
		{"empty (no versioning)", ""},
		{"null (versioning suspended)", "null"},
		{"uuid (versioning enabled)", "01HXYZ7G3K9Q2J6V8M4N5P0RAB"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := &Manifest{
				Class:     "STANDARD",
				Size:      1234,
				ChunkSize: DefaultChunkSize,
				ETag:      "deadbeef",
				BackendRef: &BackendRef{
					Backend:   "s3",
					Key:       "bucket-uuid/object-uuid",
					ETag:      "deadbeef",
					Size:      1234,
					VersionID: tc.versionID,
				},
			}
			b, err := EncodeManifest(in)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			out, err := DecodeManifest(b)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !reflect.DeepEqual(in, out) {
				t.Fatalf("round-trip mismatch:\n in: %+v\nout: %+v", in.BackendRef, out.BackendRef)
			}
			if out.BackendRef.VersionID != tc.versionID {
				t.Fatalf("VersionID lost: want %q, got %q", tc.versionID, out.BackendRef.VersionID)
			}
		})
	}
}

func TestEmptyVersionIDOmittedFromJSON(t *testing.T) {
	in := &Manifest{
		BackendRef: &BackendRef{Backend: "s3", Key: "k", ETag: "e", Size: 1},
	}
	b, err := EncodeManifest(in)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if strings.Contains(string(b), "VersionID") {
		t.Fatalf("empty VersionID must be omitted from JSON; got %s", b)
	}
}

func TestLegacyRadosManifestDecodesWithoutBackendRef(t *testing.T) {
	// Hand-crafted JSON in the pre-US-008 shape (no BackendRef field).
	// This exact byte sequence is what existing RADOS-mode rows look like
	// in Cassandra blobs / TiKV values; decode must succeed with
	// BackendRef = nil and Chunks intact.
	legacy := []byte(`{
		"Class": "STANDARD",
		"Size": 4194304,
		"ChunkSize": 4194304,
		"ETag": "abc",
		"Chunks": [{
			"Cluster": "ceph",
			"Pool": "strata.rgw.buckets.data",
			"OID": "obj-1",
			"Size": 4194304
		}]
	}`)
	out, err := DecodeManifest(legacy)
	if err != nil {
		t.Fatalf("decode legacy: %v", err)
	}
	if out == nil {
		t.Fatal("decode legacy: nil manifest")
	}
	if out.BackendRef != nil {
		t.Fatalf("legacy manifest must decode with BackendRef=nil, got %+v", out.BackendRef)
	}
	if len(out.Chunks) != 1 || out.Chunks[0].OID != "obj-1" {
		t.Fatalf("legacy Chunks lost: %+v", out.Chunks)
	}
}

func TestRoundTripPartChunks(t *testing.T) {
	in := &Manifest{
		Class:     "STANDARD",
		Size:      15 * 1024 * 1024,
		ChunkSize: DefaultChunkSize,
		ETag:      "abc-3",
		PartChunks: []PartRange{
			{PartNumber: 1, Offset: 0, Size: 5 * 1024 * 1024, ETag: "p1"},
			{PartNumber: 2, Offset: 5 * 1024 * 1024, Size: 5 * 1024 * 1024, ETag: "p2", ChecksumValue: "v2", ChecksumAlgorithm: "SHA256"},
			{PartNumber: 3, Offset: 10 * 1024 * 1024, Size: 5 * 1024 * 1024, ETag: "p3"},
		},
	}
	b, err := EncodeManifest(in)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	out, err := DecodeManifest(b)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !reflect.DeepEqual(in.PartChunks, out.PartChunks) {
		t.Fatalf("PartChunks round-trip mismatch:\n in: %+v\nout: %+v", in.PartChunks, out.PartChunks)
	}
}

func TestNilPartChunksOmittedFromJSON(t *testing.T) {
	in := &Manifest{
		Class: "STANDARD",
		Size:  10,
		ETag:  "e",
	}
	b, err := EncodeManifest(in)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if strings.Contains(string(b), "PartChunks") {
		t.Fatalf("nil PartChunks must be omitted from JSON; got %s", b)
	}
}

func TestLegacyManifestDecodesWithNilPartChunks(t *testing.T) {
	// Pre-US-001-of-s3-tests-90 manifests do not carry PartChunks. They
	// must still decode with PartChunks=nil so existing rows serve normally.
	legacy := []byte(`{"Class":"STANDARD","Size":10,"ETag":"e","Chunks":[{"Cluster":"c","Pool":"p","OID":"o","Size":10}]}`)
	out, err := DecodeManifest(legacy)
	if err != nil {
		t.Fatalf("decode legacy: %v", err)
	}
	if out.PartChunks != nil {
		t.Fatalf("legacy manifest must decode with PartChunks=nil, got %+v", out.PartChunks)
	}
}

func TestS3ManifestDecodesWithEmptyChunks(t *testing.T) {
	in := &Manifest{
		Class:      "STANDARD",
		Size:       100,
		ETag:       "e",
		BackendRef: &BackendRef{Backend: "s3", Key: "k", ETag: "e", Size: 100, VersionID: "v"},
	}
	b, err := EncodeManifest(in)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	out, err := DecodeManifest(b)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Chunks) != 0 {
		t.Fatalf("S3 manifest must round-trip with empty Chunks, got %+v", out.Chunks)
	}
	if out.BackendRef == nil {
		t.Fatal("S3 manifest must round-trip with BackendRef set")
	}
}
