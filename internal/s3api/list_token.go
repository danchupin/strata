package s3api

import (
	"encoding/base64"
	"encoding/json"
)

// v2ContinuationToken is the inner payload of an opaque
// ListObjectsV2 continuation token. The wire form is
// base64(JSON(v2ContinuationToken)). AWS keeps the token opaque so
// clients only round-trip it; we follow the same shape so a literal
// marker that is not valid base64-JSON still works (back-compat).
//
// Shard is reserved for future per-shard cursoring (cassandra
// ListObjects currently uses a single global Marker).
type v2ContinuationToken struct {
	Marker string `json:"m"`
	Shard  int    `json:"s,omitempty"`
}

func encodeContinuationToken(marker string) string {
	if marker == "" {
		return ""
	}
	b, err := json.Marshal(v2ContinuationToken{Marker: marker})
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// decodeContinuationToken parses an opaque V2 continuation token. ok
// is false when the token is not valid base64-JSON; callers should
// fall back to treating the input as a literal marker so in-flight
// pre-cycle clients (and any tooling that hand-crafted markers) keep
// paging.
func decodeContinuationToken(token string) (marker string, ok bool) {
	if token == "" {
		return "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		raw, err = base64.StdEncoding.DecodeString(token)
		if err != nil {
			return "", false
		}
	}
	var t v2ContinuationToken
	if err := json.Unmarshal(raw, &t); err != nil {
		return "", false
	}
	return t.Marker, true
}
