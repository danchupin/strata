package s3api_test

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/xml"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestObjectPutGetDelete(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	h.mustStatus(h.doString("PUT", "/bkt/greet.txt", "hello"), 200)

	resp := h.doString("GET", "/bkt/greet.txt", "")
	h.mustStatus(resp, 200)
	if body := h.readBody(resp); body != "hello" {
		t.Errorf("GET body: got %q want %q", body, "hello")
	}

	resp = h.doString("HEAD", "/bkt/greet.txt", "")
	h.mustStatus(resp, 200)
	if resp.Header.Get("Etag") == "" {
		t.Errorf("HEAD missing ETag header")
	}

	h.mustStatus(h.doString("DELETE", "/bkt/greet.txt", ""), 204)
	h.mustStatus(h.doString("GET", "/bkt/greet.txt", ""), 404)
}

func TestObjectLargeRoundTrip(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	src := make([]byte, 9<<20)
	if _, err := rand.Read(src); err != nil {
		t.Fatal(err)
	}

	h.mustStatus(h.do("PUT", "/bkt/big.bin", bytes.NewReader(src)), 200)

	resp := h.doString("GET", "/bkt/big.bin", "")
	h.mustStatus(resp, 200)
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(src, got) {
		t.Fatalf("body mismatch: want %d bytes, got %d", len(src), len(got))
	}
}

func TestObjectRangeGet(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt/k", "0123456789"), 200)

	cases := []struct {
		name       string
		hdr        string
		wantStatus int
		wantBody   string
		wantCR     string
	}{
		{"range-0-4", "bytes=0-4", http.StatusPartialContent, "01234", "bytes 0-4/10"},
		{"range-3-7", "bytes=3-7", http.StatusPartialContent, "34567", "bytes 3-7/10"},
		{"range-5-", "bytes=5-", http.StatusPartialContent, "56789", "bytes 5-9/10"},
		{"range-suffix-3", "bytes=-3", http.StatusPartialContent, "789", "bytes 7-9/10"},
		{"no-range", "", http.StatusOK, "0123456789", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			headers := []string{}
			if tc.hdr != "" {
				headers = append(headers, "Range", tc.hdr)
			}
			resp := h.doString("GET", "/bkt/k", "", headers...)
			h.mustStatus(resp, tc.wantStatus)
			if cr := resp.Header.Get("Content-Range"); cr != tc.wantCR {
				t.Errorf("Content-Range: got %q want %q", cr, tc.wantCR)
			}
			if body := h.readBody(resp); body != tc.wantBody {
				t.Errorf("body: got %q want %q", body, tc.wantBody)
			}
		})
	}
}

func TestListObjectsPrefixDelimiter(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	for _, k := range []string{"a.txt", "logs/2026/01/a.log", "logs/2026/01/b.log", "logs/2026/02/c.log"} {
		h.mustStatus(h.doString("PUT", "/bkt/"+k, "x"), 200)
	}

	resp := h.doString("GET", "/bkt?list-type=2&prefix=logs/&delimiter=/", "")
	h.mustStatus(resp, 200)
	body := h.readBody(resp)
	if !strings.Contains(body, "<CommonPrefixes><Prefix>logs/2026/</Prefix></CommonPrefixes>") {
		t.Errorf("expected CommonPrefix logs/2026/, got: %s", body)
	}

	resp = h.doString("GET", "/bkt?list-type=2&prefix=logs/2026/01/", "")
	h.mustStatus(resp, 200)
	body = h.readBody(resp)
	if !strings.Contains(body, "<Key>logs/2026/01/a.log</Key>") ||
		!strings.Contains(body, "<Key>logs/2026/01/b.log</Key>") ||
		strings.Contains(body, "<Key>logs/2026/02/c.log</Key>") {
		t.Errorf("prefix filter broken: %s", body)
	}
}

// TestListObjectsDelimiterPrefixV1 mirrors the s3-tests
// `test_bucket_list_delimiter_prefix` flow: 5 keys, delim='/', step through
// pages of size 1 and 2 with various marker / prefix combinations and assert
// the AWS-spec NextMarker shape (last emitted item, not the next-unadded one)
// + correct CommonPrefix dedup against the marker.
func TestListObjectsDelimiterPrefixV1(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	for _, k := range []string{"asdf", "boo/bar", "boo/baz/xyzzy", "cquux/thud", "cquux/bla"} {
		h.mustStatus(h.doString("PUT", "/bkt/"+k, "x"), 200)
	}

	type want struct {
		truncated  bool
		objects    []string
		prefixes   []string
		nextMarker string
	}
	step := func(t *testing.T, prefix, marker string, maxKeys int, w want) string {
		t.Helper()
		path := "/bkt?delimiter=/&prefix=" + prefix + "&marker=" + marker + "&max-keys=" +
			itoa(maxKeys)
		resp := h.doString("GET", path, "")
		h.mustStatus(resp, 200)
		got := parseListV1(t, h.readBody(resp))
		if got.IsTruncated != w.truncated {
			t.Errorf("truncated: got %v want %v", got.IsTruncated, w.truncated)
		}
		if !equalStrings(got.Keys(), w.objects) {
			t.Errorf("objects: got %v want %v", got.Keys(), w.objects)
		}
		if !equalStrings(got.Prefixes(), w.prefixes) {
			t.Errorf("prefixes: got %v want %v", got.Prefixes(), w.prefixes)
		}
		if got.NextMarker != w.nextMarker {
			t.Errorf("NextMarker: got %q want %q", got.NextMarker, w.nextMarker)
		}
		return got.NextMarker
	}

	// prefix=''
	m := step(t, "", "", 1, want{true, []string{"asdf"}, nil, "asdf"})
	m = step(t, "", m, 1, want{true, nil, []string{"boo/"}, "boo/"})
	m = step(t, "", m, 1, want{false, nil, []string{"cquux/"}, ""})

	m = step(t, "", "", 2, want{true, []string{"asdf"}, []string{"boo/"}, "boo/"})
	_ = step(t, "", m, 2, want{false, nil, []string{"cquux/"}, ""})

	// prefix='boo/'
	m = step(t, "boo/", "", 1, want{true, []string{"boo/bar"}, nil, "boo/bar"})
	m = step(t, "boo/", m, 1, want{false, nil, []string{"boo/baz/"}, ""})
	_ = step(t, "boo/", "", 2, want{false, []string{"boo/bar"}, []string{"boo/baz/"}, ""})
}

// TestListObjectsDelimiterPrefixEndsWithDelimiter mirrors
// `test_bucket_list_delimiter_prefix_ends_with_delimiter`: a single key
// `asdf/` with prefix=`asdf/` + delim=`/` returns the key as an object
// (rest after stripping prefix is empty — no delimiter inside).
func TestListObjectsDelimiterPrefixEndsWithDelimiter(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt/asdf/", "x"), 200)

	resp := h.doString("GET", "/bkt?delimiter=/&prefix=asdf/&max-keys=1000", "")
	h.mustStatus(resp, 200)
	got := parseListV1(t, h.readBody(resp))
	if got.IsTruncated {
		t.Errorf("truncated should be false, got %v", got.IsTruncated)
	}
	if !equalStrings(got.Keys(), []string{"asdf/"}) {
		t.Errorf("objects: got %v want [asdf/]", got.Keys())
	}
	if len(got.Prefixes()) != 0 {
		t.Errorf("prefixes: got %v want empty", got.Prefixes())
	}
	if got.NextMarker != "" {
		t.Errorf("NextMarker: got %q want empty", got.NextMarker)
	}
}

// listV1Parsed is a flat decode of <ListBucketResult> for V1 listing tests.
type listV1Parsed struct {
	XMLName     xml.Name `xml:"ListBucketResult"`
	IsTruncated bool     `xml:"IsTruncated"`
	NextMarker  string   `xml:"NextMarker"`
	Contents    []struct {
		Key string `xml:"Key"`
	} `xml:"Contents"`
	CommonPrefixes []struct {
		Prefix string `xml:"Prefix"`
	} `xml:"CommonPrefixes"`
}

func (l *listV1Parsed) Keys() []string {
	out := make([]string, 0, len(l.Contents))
	for _, c := range l.Contents {
		out = append(out, c.Key)
	}
	return out
}

func (l *listV1Parsed) Prefixes() []string {
	out := make([]string, 0, len(l.CommonPrefixes))
	for _, p := range l.CommonPrefixes {
		out = append(out, p.Prefix)
	}
	return out
}

func parseListV1(t *testing.T, body string) *listV1Parsed {
	t.Helper()
	var out listV1Parsed
	if err := xml.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("parseListV1: %v: body=%s", err, body)
	}
	return &out
}

func equalStrings(a, b []string) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// listV2Parsed is a flat decode of <ListBucketResult> for V2 listing tests.
type listV2Parsed struct {
	XMLName               xml.Name `xml:"ListBucketResult"`
	IsTruncated           bool     `xml:"IsTruncated"`
	ContinuationToken     string   `xml:"ContinuationToken"`
	NextContinuationToken string   `xml:"NextContinuationToken"`
	KeyCount              int      `xml:"KeyCount"`
	Contents              []struct {
		Key string `xml:"Key"`
	} `xml:"Contents"`
	CommonPrefixes []struct {
		Prefix string `xml:"Prefix"`
	} `xml:"CommonPrefixes"`
}

func (l *listV2Parsed) Keys() []string {
	out := make([]string, 0, len(l.Contents))
	for _, c := range l.Contents {
		out = append(out, c.Key)
	}
	return out
}

func parseListV2(t *testing.T, body string) *listV2Parsed {
	t.Helper()
	var out listV2Parsed
	if err := xml.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("parseListV2: %v: body=%s", err, body)
	}
	return &out
}

// TestListObjectsV2OpaqueContinuationToken verifies that
// NextContinuationToken is base64-URL of a JSON struct (not a literal
// marker) and that round-tripping the token paginates through every key
// exactly once.
func TestListObjectsV2OpaqueContinuationToken(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	keys := []string{"k1", "k2", "k3", "k4", "k5"}
	for _, k := range keys {
		h.mustStatus(h.doString("PUT", "/bkt/"+k, "x"), 200)
	}

	var seen []string
	token := ""
	for i := 0; i < 10; i++ {
		path := "/bkt?list-type=2&max-keys=2"
		if token != "" {
			path += "&continuation-token=" + url.QueryEscape(token)
		}
		resp := h.doString("GET", path, "")
		h.mustStatus(resp, 200)
		got := parseListV2(t, h.readBody(resp))
		seen = append(seen, got.Keys()...)
		if !got.IsTruncated {
			if got.NextContinuationToken != "" {
				t.Fatalf("non-truncated page leaked NextContinuationToken=%q", got.NextContinuationToken)
			}
			break
		}
		if got.NextContinuationToken == "" {
			t.Fatalf("truncated page missing NextContinuationToken")
		}
		// Token must be opaque base64-URL JSON, NOT a literal key.
		raw, err := base64.RawURLEncoding.DecodeString(got.NextContinuationToken)
		if err != nil {
			t.Fatalf("NextContinuationToken not base64-RawURL: %v (token=%q)", err, got.NextContinuationToken)
		}
		if !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) {
			t.Fatalf("decoded token not JSON: raw=%q", raw)
		}
		token = got.NextContinuationToken
	}
	if !equalStrings(seen, keys) {
		t.Fatalf("paged keys: got %v want %v", seen, keys)
	}
}

// TestListObjectsV2LegacyLiteralTokenFallback covers backward compat: a
// caller passing a pre-US-006 literal-marker token continues paging
// from that key without error.
func TestListObjectsV2LegacyLiteralTokenFallback(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	for _, k := range []string{"k1", "k2", "k3", "k4"} {
		h.mustStatus(h.doString("PUT", "/bkt/"+k, "x"), 200)
	}

	resp := h.doString("GET", "/bkt?list-type=2&continuation-token="+url.QueryEscape("k2"), "")
	h.mustStatus(resp, 200)
	got := parseListV2(t, h.readBody(resp))
	// Marker semantics: emit keys strictly greater than k2.
	if !equalStrings(got.Keys(), []string{"k3", "k4"}) {
		t.Errorf("legacy-literal marker: got %v want [k3 k4]", got.Keys())
	}
}

func TestStorageClassHeader(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt/k", "x", "x-amz-storage-class", "STANDARD_IA"), 200)

	resp := h.doString("HEAD", "/bkt/k", "")
	h.mustStatus(resp, 200)
	if sc := resp.Header.Get("X-Amz-Storage-Class"); sc != "STANDARD_IA" {
		t.Errorf("storage-class: got %q want STANDARD_IA", sc)
	}
}
