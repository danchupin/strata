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
	// SSE records the encryption disposition chosen at write time (US-013).
	// nil for legacy manifests written before US-013 — those decode as
	// passthrough/none, identical to the pre-flag behaviour.
	SSE *SSEInfo `json:",omitempty"`
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

// SSE encryption modes (US-013). Recorded per-object in Manifest.SSE.Mode so
// the GET path can decrypt according to how the object was written, not the
// current backend config (which may have been flipped after the write).
const (
	// SSEModePassthrough is the default: every Strata Put forwards an
	// x-amz-server-side-encryption header to the backend. Encryption-at-rest
	// is the backend's job; bytes leave Strata in cleartext on the wire to
	// the backend and the backend stores ciphertext.
	SSEModePassthrough = "passthrough"
	// SSEModeStrata applies Strata's own envelope encryption (SSE-S3 /
	// SSE-KMS) before the bytes hit the backend. The backend stores the
	// already-encrypted bytes as plain application/octet-stream — no
	// backend-side SSE header. Reserved for compliance setups that want
	// keys never visible to the storage tier; gateway-side envelope
	// encryption is plumbed by US-013 but not yet implemented here, so in
	// this mode bytes pass through unmodified — the no-backend-SSE
	// behaviour is the load-bearing observable.
	SSEModeStrata = "strata"
	// SSEModeBoth runs Strata's envelope encryption AND backend SSE, for
	// regimes that mandate two independent encryption boundaries.
	SSEModeBoth = "both"
)

// SSE algorithm tags — pinned to the AWS S3 wire vocabulary so they round-trip
// through the SDK's typed enum without translation.
const (
	SSEAlgorithmAES256 = "AES256"
	SSEAlgorithmKMS    = "aws:kms"
)

// SSEInfo records the encryption disposition of a single Strata object at
// write time. Persisted on the Manifest so future GET / re-PUT paths can
// branch on the mode that produced the object regardless of the backend's
// current configuration.
//
// Fields:
//   - Mode is one of SSEMode{Passthrough,Strata,Both}.
//   - Algorithm captures the wire SSE algorithm sent to the backend
//     (SSEAlgorithmAES256 / SSEAlgorithmKMS), or empty when no backend SSE
//     header was sent (SSEModeStrata).
//   - KMSKeyID carries the resolved KMS key id when Algorithm == aws:kms.
type SSEInfo struct {
	Mode      string `json:",omitempty"`
	Algorithm string `json:",omitempty"`
	KMSKeyID  string `json:",omitempty"`
}
