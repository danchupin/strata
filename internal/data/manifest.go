package data

import "encoding/json"

const DefaultChunkSize int64 = 4 * 1024 * 1024

// EncodeManifest serialises a Manifest to its on-the-wire JSON form. Used by
// meta backends that store the manifest as opaque bytes (Cassandra blob,
// TiKV value). Returns nil for a nil input so callers can blindly delegate.
func EncodeManifest(m *Manifest) ([]byte, error) {
	if m == nil {
		return nil, nil
	}
	return json.Marshal(m)
}

// DecodeManifest is the inverse of EncodeManifest. An empty input yields a
// nil manifest (consistent with EncodeManifest(nil)).
func DecodeManifest(b []byte) (*Manifest, error) {
	if len(b) == 0 {
		return nil, nil
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// Manifest describes how an object's bytes are stored in the data plane.
//
// Native shape (RADOS): bytes are split into Chunks []ChunkRef and Chunks is
// non-empty; BackendRef is nil.
//
// S3-backend shape (US-008): one Strata object = one backend S3 object;
// BackendRef is non-nil and Chunks is empty. The 1:1 mapping is invariant —
// callers MUST NOT populate both Chunks and BackendRef on the same manifest.
type Manifest struct {
	Class      string
	Size       int64
	ChunkSize  int64
	ETag       string
	Chunks     []ChunkRef
	BackendRef *BackendRef `json:",omitempty"`
}

type ChunkRef struct {
	Cluster   string
	Pool      string
	Namespace string `json:",omitempty"`
	OID       string
	Size      int64
}

// BackendRef points at a single object in an external object-store backend
// (currently the S3-over-S3 data backend). When set, the whole Strata object
// lives at Key in the configured backend bucket; Manifest.Chunks is empty.
//
// VersionID semantics (defensive — three documented shapes):
//   - "" (empty): backend does not support versioning, OR versioning is off,
//     OR the SDK response did not carry a version-id. Plain DeleteObject
//     (no VersionId) frees bytes immediately.
//   - "null" (literal four-character string): backend versioning is
//     Suspended. A versioned DeleteObject with VersionId="null" cleans the
//     suspended slot without creating a delete-marker.
//   - any other non-empty string (UUID-shaped): backend versioning is
//     Enabled. A versioned DeleteObject with the captured VersionId deletes
//     the specific version and bypasses delete-marker creation.
//
// VersionID is captured from the SDK response at PUT/CompleteMultipart time
// and passed back to the SDK on Delete. Strata does NOT branch on backend
// capability — it stores whatever the SDK returned and replays it verbatim.
type BackendRef struct {
	Backend   string `json:",omitempty"`
	Key       string `json:",omitempty"`
	ETag      string `json:",omitempty"`
	Size      int64  `json:",omitempty"`
	VersionID string `json:",omitempty"`
}
