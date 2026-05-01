package s3api_test

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"testing"
)

// US-006: V2 ContinuationToken is opaque (base64 of JSON {Marker, Shard}).
// Verify the wire form: NextContinuationToken decodes to base64 → JSON →
// {Marker:"<lastEmitted>"}.
func TestListV2ContinuationTokenIsOpaque(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	for _, k := range []string{"a", "b", "c"} {
		h.mustStatus(h.doString("PUT", "/bkt/"+k, "x"), 200)
	}

	q := url.Values{}
	q.Set("list-type", "2")
	q.Set("max-keys", "1")
	resp := h.doString("GET", "/bkt?"+q.Encode(), "")
	h.mustStatus(resp, 200)
	r := decodeV2(t, h.readBody(resp))
	if !r.IsTruncated || r.NextContinuationToken == "" {
		t.Fatalf("p1: want truncated+token, got %#v", r)
	}
	if r.NextContinuationToken == "a" {
		t.Fatalf("token must be opaque, not literal marker: %q", r.NextContinuationToken)
	}
	raw, err := base64.RawURLEncoding.DecodeString(r.NextContinuationToken)
	if err != nil {
		t.Fatalf("token not RawURL-base64: %v", err)
	}
	var payload struct {
		Marker string `json:"m"`
		Shard  int    `json:"s"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("token not JSON: %v body=%s", err, raw)
	}
	if payload.Marker != "a" {
		t.Fatalf("token marker=%q want %q", payload.Marker, "a")
	}
}

// Round-trip: client supplies the opaque NextContinuationToken back as
// ContinuationToken; pagination resumes correctly. Also verifies that
// ContinuationToken in the response echoes the raw client token.
func TestListV2ContinuationTokenRoundTrip(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	for _, k := range []string{"a", "b", "c"} {
		h.mustStatus(h.doString("PUT", "/bkt/"+k, "x"), 200)
	}
	page := func(token string) listV2Resp {
		t.Helper()
		q := url.Values{}
		q.Set("list-type", "2")
		q.Set("max-keys", "1")
		if token != "" {
			q.Set("continuation-token", token)
		}
		resp := h.doString("GET", "/bkt?"+q.Encode(), "")
		h.mustStatus(resp, 200)
		return decodeV2(t, h.readBody(resp))
	}
	r1 := page("")
	if !eqSlice(keysOfV2(r1), []string{"a"}) {
		t.Fatalf("p1: %#v", r1)
	}
	r2 := page(r1.NextContinuationToken)
	if !eqSlice(keysOfV2(r2), []string{"b"}) || r2.ContinuationToken != r1.NextContinuationToken {
		t.Fatalf("p2: keys=%v echoed=%q want=%q", keysOfV2(r2), r2.ContinuationToken, r1.NextContinuationToken)
	}
	r3 := page(r2.NextContinuationToken)
	if !eqSlice(keysOfV2(r3), []string{"c"}) || r3.IsTruncated || r3.NextContinuationToken != "" {
		t.Fatalf("p3: %#v", r3)
	}
}

// Back-compat: a non-base64-JSON continuation token (e.g. an in-flight
// pre-cycle literal marker) must still be honoured as a literal marker
// so paging in flight doesn't break.
func TestListV2ContinuationTokenBackCompatLiteral(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	for _, k := range []string{"a", "b", "c"} {
		h.mustStatus(h.doString("PUT", "/bkt/"+k, "x"), 200)
	}
	q := url.Values{}
	q.Set("list-type", "2")
	q.Set("max-keys", "1")
	q.Set("continuation-token", "a") // literal, not base64-JSON
	resp := h.doString("GET", "/bkt?"+q.Encode(), "")
	h.mustStatus(resp, 200)
	r := decodeV2(t, h.readBody(resp))
	if !eqSlice(keysOfV2(r), []string{"b"}) {
		t.Fatalf("back-compat literal: %#v", r)
	}
}

// V1 marker parameter unchanged — opaque tokens are V2-only. A V1
// request with `?marker=a` keeps treating "a" as a literal key.
func TestListV1MarkerUnchanged(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	for _, k := range []string{"a", "b", "c"} {
		h.mustStatus(h.doString("PUT", "/bkt/"+k, "x"), 200)
	}
	q := url.Values{}
	q.Set("max-keys", "1")
	q.Set("marker", "a")
	resp := h.doString("GET", "/bkt?"+q.Encode(), "")
	h.mustStatus(resp, 200)
	r := decodeV1(t, h.readBody(resp))
	if !eqSlice(keysOf(r), []string{"b"}) {
		t.Fatalf("v1 marker literal: %#v", r)
	}
}
