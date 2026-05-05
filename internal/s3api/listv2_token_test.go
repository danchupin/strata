package s3api

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestListV2TokenRoundTrip(t *testing.T) {
	cases := []string{
		"",
		"a",
		"some/prefix/with/slashes",
		"weird\x01\x02bytes",
		strings.Repeat("x", 1024),
	}
	for _, m := range cases {
		got := decodeListV2Token(encodeListV2Token(m))
		if m == "" {
			if got != "" {
				t.Errorf("empty: got %q want empty", got)
			}
			continue
		}
		if got != m {
			t.Errorf("round-trip: got %q want %q", got, m)
		}
	}
}

func TestListV2TokenEncodedShape(t *testing.T) {
	enc := encodeListV2Token("foo")
	raw, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil {
		t.Fatalf("not base64-RawURL: %v (token=%q)", err, enc)
	}
	var tok listV2Token
	if err := json.Unmarshal(raw, &tok); err != nil {
		t.Fatalf("inner not JSON: %v", err)
	}
	if tok.Marker != "foo" {
		t.Errorf("Marker: got %q want foo", tok.Marker)
	}
}

// TestListV2TokenLegacyLiteralFallback covers the backward-compat path:
// pre-US-006 clients will be holding a literal-marker token mid-page.
// decodeListV2Token must return the input verbatim when it isn't a valid
// base64-JSON listV2Token.
func TestListV2TokenLegacyLiteralFallback(t *testing.T) {
	cases := []string{
		"plain-marker",
		"some/path/with/slashes",
		"a=b&c=d",
		"contains spaces",
		"!@#$%^&*()",
	}
	for _, m := range cases {
		if got := decodeListV2Token(m); got != m {
			t.Errorf("literal fallback %q: got %q", m, got)
		}
	}
}

// TestListV2TokenRejectsBase64WithEmptyMarker: a valid base64-JSON whose
// inner Marker is empty falls back to the literal-input interpretation,
// which means even a JSON-but-empty marker won't silently advance to "".
func TestListV2TokenRejectsBase64WithEmptyMarker(t *testing.T) {
	enc := base64.RawURLEncoding.EncodeToString([]byte(`{"m":""}`))
	if got := decodeListV2Token(enc); got != enc {
		t.Errorf("empty-inner-marker: got %q want literal %q", got, enc)
	}
}
