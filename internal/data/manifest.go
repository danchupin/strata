package data

import (
	"encoding/json"
)

const DefaultChunkSize int64 = 4 * 1024 * 1024

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
	// SSE records the encryption disposition chosen at write time (US-013).
	SSE *SSEInfo `json:",omitempty"`
	// PartChunks records the per-part byte range of a multipart upload, in
	// part order. Used by ?partNumber=N GET (US-001 / US-002) to serve the
	// exact part body without scanning. Nil for single-PUT objects (the
	// nil-vs-empty distinction is the "not multipart" sentinel).
	PartChunks []PartRange `json:",omitempty"`
	// PartChunkCounts records the number of data-backend chunks contributed
	// by each part of a multipart upload, in part order. Used by the SSE-S3
	// multipart decrypt locator (chunk index ↔ part lookup). Empty for
	// single-PUT objects. Pre-US-001 rows JSON-encoded this field as
	// "PartChunks"; UnmarshalJSON sniffs and migrates so old rows keep
	// decoding even after the field rename.
	PartChunkCounts []int `json:",omitempty"`
	// PartChecksums records the per-part stored x-amz-checksum-<algo>
	// values in PartNumber order. Empty for single-PUT objects.
	PartChecksums []map[string]string `json:",omitempty"`
}

// PartRange describes one part of a multipart upload at byte-range
// granularity. Offset/Size are in object plaintext space (so ?partNumber=N
// GET maps directly to bytes [Offset, Offset+Size) of the object). ETag,
// ChecksumValue, and ChecksumAlgorithm carry the part-level metadata
// surfaced by HEAD ?partNumber=N + ChecksumMode=ENABLED.
type PartRange struct {
	PartNumber        int    `json:",omitempty"`
	Offset            int64  `json:",omitempty"`
	Size              int64  `json:",omitempty"`
	ETag              string `json:",omitempty"`
	ChecksumValue     string `json:",omitempty"`
	ChecksumAlgorithm string `json:",omitempty"`
}

// UnmarshalJSON migrates pre-US-001 manifests where "PartChunks" was a
// []int (chunk-counts shape) into the new PartChunkCounts field. New rows
// where "PartChunks" is a []PartRange decode straight into PartChunks. The
// rest of the struct decodes via the type-alias trick that suppresses
// recursion into this method.
func (m *Manifest) UnmarshalJSON(b []byte) error {
	type alias Manifest
	aux := &struct {
		PartChunks json.RawMessage `json:"PartChunks,omitempty"`
		*alias
	}{alias: (*alias)(m)}
	if err := json.Unmarshal(b, aux); err != nil {
		return err
	}
	m.PartChunks = nil
	if len(aux.PartChunks) == 0 {
		return nil
	}
	var ranges []PartRange
	if err := json.Unmarshal(aux.PartChunks, &ranges); err == nil {
		m.PartChunks = ranges
		return nil
	}
	var counts []int
	if err := json.Unmarshal(aux.PartChunks, &counts); err != nil {
		return err
	}
	m.PartChunkCounts = counts
	return nil
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
//     OR the SDK response did not carry a version-id.
//   - "null" (literal four-character string): backend versioning is Suspended.
//   - any other non-empty string (UUID-shaped): backend versioning is Enabled.
type BackendRef struct {
	Backend   string `json:",omitempty"`
	Key       string `json:",omitempty"`
	ETag      string `json:",omitempty"`
	Size      int64  `json:",omitempty"`
	VersionID string `json:",omitempty"`
}

// SSE encryption modes (US-013).
const (
	SSEModePassthrough = "passthrough"
	SSEModeStrata      = "strata"
	SSEModeBoth        = "both"
)

const (
	SSEAlgorithmAES256 = "AES256"
	SSEAlgorithmKMS    = "aws:kms"
)

// SSEInfo records the encryption disposition of a single Strata object at
// write time. Persisted on the Manifest so future GET / re-PUT paths can
// branch on the mode that produced the object regardless of the backend's
// current configuration.
type SSEInfo struct {
	Mode      string `json:",omitempty"`
	Algorithm string `json:",omitempty"`
	KMSKeyID  string `json:",omitempty"`
}
