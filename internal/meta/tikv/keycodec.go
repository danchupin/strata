// Package tikv shared key + blob codec helpers (US-001 tikv-stubs).
//
// keys.go owns the entity-specific key constructors (BucketKey, ObjectKey, …);
// this file owns the generic primitives stories US-001..US-005 reuse so the
// FoundationDB byte-stuffing pattern and the JSON-blob wrapping stay
// consistent across new key shapes that don't fit one of the per-entity
// constructors above.
//
// Conventions:
//   - PackKey: variable-length string / []byte segments are byte-stuffed
//     FoundationDB-style (0x00 → 0x00 0xFF, terminator 0x00 0x00) so lex
//     ordering survives heterogeneous lengths; uuid.UUID segments are
//     encoded as raw 16 bytes (fixed length, no stuffing required).
//   - MarshalBlob / UnmarshalBlob: thin wrappers around encoding/json so
//     payloads stay human-debuggable + schema-additive.
package tikv

import (
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
)

// PackKey returns prefix followed by each segment encoded per the rules in
// the package doc. Supported segment types: string, []byte, uuid.UUID. Any
// other type panics — callers must keep encodings type-safe; runtime
// surprises here would surface as silent key collisions.
//
// PackKey is the helper US-001..US-005 reach for when a new key shape does
// not fit one of the entity-specific constructors in keys.go (BucketKey,
// ObjectKey, …). Prefer the entity constructors when one applies — they
// document the layout in one place and keep range-scan call-sites
// type-safe.
func PackKey(prefix string, segments ...any) []byte {
	out := make([]byte, 0, len(prefix)+16*len(segments))
	out = append(out, prefix...)
	for _, seg := range segments {
		switch v := seg.(type) {
		case string:
			out = appendEscaped(out, v)
		case []byte:
			out = appendEscapedBytes(out, v)
		case uuid.UUID:
			out = append(out, v[:]...)
		default:
			panic(fmt.Sprintf("tikv keycodec: PackKey segment %d has unsupported type %T", len(out), seg))
		}
	}
	return out
}

// appendEscapedBytes mirrors appendEscaped but consumes []byte without the
// implicit string conversion (which would copy on every call). Same byte-
// stuffing rules.
func appendEscapedBytes(dst, b []byte) []byte {
	for _, c := range b {
		if c == 0x00 {
			dst = append(dst, 0x00, 0xFF)
		} else {
			dst = append(dst, c)
		}
	}
	return append(dst, 0x00, 0x00)
}

// MarshalBlob serialises v as JSON for persistence as the value half of a
// key/value pair. Returning a typed wrapper keeps the call-sites uniform
// even though the body is one line — future stories can swap the encoder
// (e.g. add a length-prefix or a magic byte) without touching every caller.
func MarshalBlob(v any) ([]byte, error) {
	return json.Marshal(v)
}

// UnmarshalBlob is the inverse of MarshalBlob. Empty input is treated as a
// no-op (v left zero) rather than a JSON error so absent rows decode
// cleanly.
func UnmarshalBlob(data []byte, v any) error {
	if len(data) == 0 {
		return nil
	}
	return json.Unmarshal(data, v)
}
