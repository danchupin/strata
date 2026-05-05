package s3api

import (
	"encoding/base64"
	"encoding/json"
)

// listV2Token is the AWS-style opaque continuation token shape for
// ListObjectsV2. The encoded form is base64-URL of JSON-encoded fields.
// Shard is reserved for backends that resume mid-fan-out; the gateway
// always emits 0 today.
type listV2Token struct {
	Marker string `json:"m"`
	Shard  int    `json:"s,omitempty"`
}

// encodeListV2Token returns the opaque base64-URL form. An empty marker
// yields an empty string so callers can omit the field via XML omitempty.
func encodeListV2Token(marker string) string {
	if marker == "" {
		return ""
	}
	b, _ := json.Marshal(listV2Token{Marker: marker})
	return base64.RawURLEncoding.EncodeToString(b)
}

// decodeListV2Token parses an opaque token back into its Marker. If the
// input is not valid base64-RawURL JSON of listV2Token shape, the input
// is returned verbatim — backward compat with literal-marker tokens
// already in flight.
func decodeListV2Token(s string) string {
	if s == "" {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		// Tolerate base64-Std (with padding) clients that decoded our
		// raw-url form via a permissive decoder and re-emitted it.
		raw, err = base64.StdEncoding.DecodeString(s)
		if err != nil {
			return s
		}
	}
	var t listV2Token
	if err := json.Unmarshal(raw, &t); err != nil {
		return s
	}
	if t.Marker == "" {
		return s
	}
	return t.Marker
}
