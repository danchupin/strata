package s3api_test

import (
	"encoding/xml"
	"net/url"
	"strings"
	"testing"
)

type listV1Resp struct {
	XMLName        xml.Name `xml:"ListBucketResult"`
	Name           string   `xml:"Name"`
	Prefix         string   `xml:"Prefix"`
	Marker         string   `xml:"Marker"`
	NextMarker     string   `xml:"NextMarker"`
	MaxKeys        int      `xml:"MaxKeys"`
	Delimiter      string   `xml:"Delimiter"`
	IsTruncated    bool     `xml:"IsTruncated"`
	Contents       []struct {
		Key string `xml:"Key"`
	} `xml:"Contents"`
	CommonPrefixes []struct {
		Prefix string `xml:"Prefix"`
	} `xml:"CommonPrefixes"`
}

type listV2Resp struct {
	XMLName               xml.Name `xml:"ListBucketResult"`
	Name                  string   `xml:"Name"`
	Prefix                string   `xml:"Prefix"`
	KeyCount              int      `xml:"KeyCount"`
	MaxKeys               int      `xml:"MaxKeys"`
	IsTruncated           bool     `xml:"IsTruncated"`
	NextContinuationToken string   `xml:"NextContinuationToken"`
	ContinuationToken     string   `xml:"ContinuationToken"`
	StartAfter            string   `xml:"StartAfter"`
	Contents              []struct {
		Key string `xml:"Key"`
	} `xml:"Contents"`
	CommonPrefixes []struct {
		Prefix string `xml:"Prefix"`
	} `xml:"CommonPrefixes"`
}

func decodeV1(t *testing.T, body string) listV1Resp {
	t.Helper()
	var r listV1Resp
	if err := xml.Unmarshal([]byte(body), &r); err != nil {
		t.Fatalf("xml unmarshal: %v\nbody=%s", err, body)
	}
	return r
}

func decodeV2(t *testing.T, body string) listV2Resp {
	t.Helper()
	var r listV2Resp
	if err := xml.Unmarshal([]byte(body), &r); err != nil {
		t.Fatalf("xml unmarshal: %v\nbody=%s", err, body)
	}
	return r
}

func keysOf(r listV1Resp) []string {
	out := make([]string, 0, len(r.Contents))
	for _, c := range r.Contents {
		out = append(out, c.Key)
	}
	return out
}

func prefsOf(r listV1Resp) []string {
	out := make([]string, 0, len(r.CommonPrefixes))
	for _, p := range r.CommonPrefixes {
		out = append(out, p.Prefix)
	}
	return out
}

func keysOfV2(r listV2Resp) []string {
	out := make([]string, 0, len(r.Contents))
	for _, c := range r.Contents {
		out = append(out, c.Key)
	}
	return out
}

func prefsOfV2(r listV2Resp) []string {
	out := make([]string, 0, len(r.CommonPrefixes))
	for _, p := range r.CommonPrefixes {
		out = append(out, p.Prefix)
	}
	return out
}

func eqSlice(a, b []string) bool {
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

// Mirror s3-tests test_bucket_list_delimiter_prefix.
func TestListV1DelimiterPrefix(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	for _, k := range []string{"asdf", "boo/bar", "boo/baz/xyzzy", "cquux/thud", "cquux/bla"} {
		h.mustStatus(h.doString("PUT", "/bkt/"+k, "x"), 200)
	}

	get := func(prefix, delim, marker string, max int) listV1Resp {
		t.Helper()
		q := url.Values{}
		q.Set("prefix", prefix)
		q.Set("delimiter", delim)
		if marker != "" {
			q.Set("marker", marker)
		}
		if max > 0 {
			q.Set("max-keys", itoa(max))
		}
		resp := h.doString("GET", "/bkt?"+q.Encode(), "")
		h.mustStatus(resp, 200)
		return decodeV1(t, h.readBody(resp))
	}

	// prefix='', delim='/', max=1
	r := get("", "/", "", 1)
	if !r.IsTruncated || !eqSlice(keysOf(r), []string{"asdf"}) || len(prefsOf(r)) != 0 || r.NextMarker != "asdf" {
		t.Fatalf("page1: got truncated=%v keys=%v prefs=%v next=%q", r.IsTruncated, keysOf(r), prefsOf(r), r.NextMarker)
	}
	r = get("", "/", r.NextMarker, 1)
	if !r.IsTruncated || len(keysOf(r)) != 0 || !eqSlice(prefsOf(r), []string{"boo/"}) || r.NextMarker != "boo/" {
		t.Fatalf("page2: got truncated=%v keys=%v prefs=%v next=%q", r.IsTruncated, keysOf(r), prefsOf(r), r.NextMarker)
	}
	r = get("", "/", r.NextMarker, 1)
	if r.IsTruncated || len(keysOf(r)) != 0 || !eqSlice(prefsOf(r), []string{"cquux/"}) || r.NextMarker != "" {
		t.Fatalf("page3: got truncated=%v keys=%v prefs=%v next=%q", r.IsTruncated, keysOf(r), prefsOf(r), r.NextMarker)
	}

	// prefix='', delim='/', max=2
	r = get("", "/", "", 2)
	if !r.IsTruncated || !eqSlice(keysOf(r), []string{"asdf"}) || !eqSlice(prefsOf(r), []string{"boo/"}) || r.NextMarker != "boo/" {
		t.Fatalf("page1m2: got truncated=%v keys=%v prefs=%v next=%q", r.IsTruncated, keysOf(r), prefsOf(r), r.NextMarker)
	}
	r = get("", "/", r.NextMarker, 2)
	if r.IsTruncated || len(keysOf(r)) != 0 || !eqSlice(prefsOf(r), []string{"cquux/"}) || r.NextMarker != "" {
		t.Fatalf("page2m2: got truncated=%v keys=%v prefs=%v next=%q", r.IsTruncated, keysOf(r), prefsOf(r), r.NextMarker)
	}

	// prefix='boo/'
	r = get("boo/", "/", "", 1)
	if !r.IsTruncated || !eqSlice(keysOf(r), []string{"boo/bar"}) || len(prefsOf(r)) != 0 || r.NextMarker != "boo/bar" {
		t.Fatalf("boo p1: got truncated=%v keys=%v prefs=%v next=%q", r.IsTruncated, keysOf(r), prefsOf(r), r.NextMarker)
	}
	r = get("boo/", "/", r.NextMarker, 1)
	if r.IsTruncated || len(keysOf(r)) != 0 || !eqSlice(prefsOf(r), []string{"boo/baz/"}) || r.NextMarker != "" {
		t.Fatalf("boo p2: got truncated=%v keys=%v prefs=%v next=%q", r.IsTruncated, keysOf(r), prefsOf(r), r.NextMarker)
	}

	r = get("boo/", "/", "", 2)
	if r.IsTruncated || !eqSlice(keysOf(r), []string{"boo/bar"}) || !eqSlice(prefsOf(r), []string{"boo/baz/"}) || r.NextMarker != "" {
		t.Fatalf("boo m2: got truncated=%v keys=%v prefs=%v next=%q", r.IsTruncated, keysOf(r), prefsOf(r), r.NextMarker)
	}
}

// Mirror s3-tests test_bucket_list_delimiter_prefix_ends_with_delimiter.
func TestListV1DelimiterPrefixEndsWithDelimiter(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt/asdf%2F", "x"), 200) // key="asdf/"

	resp := h.doString("GET", "/bkt?prefix=asdf%2F&delimiter=%2F&max-keys=1000", "")
	h.mustStatus(resp, 200)
	body := h.readBody(resp)
	r := decodeV1(t, body)
	if r.IsTruncated || !eqSlice(keysOf(r), []string{"asdf/"}) || len(prefsOf(r)) != 0 || r.NextMarker != "" {
		t.Fatalf("ends-with-delim: got truncated=%v keys=%v prefs=%v next=%q body=%s",
			r.IsTruncated, keysOf(r), prefsOf(r), r.NextMarker, body)
	}
}

// Mirror s3-tests test_bucket_listv2_delimiter_prefix (V2 paginated).
// V2 NextContinuationToken is OPAQUE (US-006); the test round-trips it
// as boto would rather than asserting on its literal contents.
func TestListV2DelimiterPrefix(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	for _, k := range []string{"asdf", "boo/bar", "boo/baz/xyzzy", "cquux/thud", "cquux/bla"} {
		h.mustStatus(h.doString("PUT", "/bkt/"+k, "x"), 200)
	}

	get := func(prefix, delim, token string, max int) listV2Resp {
		t.Helper()
		q := url.Values{}
		q.Set("list-type", "2")
		q.Set("prefix", prefix)
		q.Set("delimiter", delim)
		if token != "" {
			q.Set("continuation-token", token)
		}
		if max > 0 {
			q.Set("max-keys", itoa(max))
		}
		resp := h.doString("GET", "/bkt?"+q.Encode(), "")
		h.mustStatus(resp, 200)
		return decodeV2(t, h.readBody(resp))
	}

	r := get("", "/", "", 1)
	if !r.IsTruncated || !eqSlice(keysOfV2(r), []string{"asdf"}) || len(prefsOfV2(r)) != 0 || r.NextContinuationToken == "" {
		t.Fatalf("v2 p1: %#v", r)
	}
	r = get("", "/", r.NextContinuationToken, 1)
	if !r.IsTruncated || len(keysOfV2(r)) != 0 || !eqSlice(prefsOfV2(r), []string{"boo/"}) || r.NextContinuationToken == "" {
		t.Fatalf("v2 p2: %#v", r)
	}
	r = get("", "/", r.NextContinuationToken, 1)
	if r.IsTruncated || len(keysOfV2(r)) != 0 || !eqSlice(prefsOfV2(r), []string{"cquux/"}) || r.NextContinuationToken != "" {
		t.Fatalf("v2 p3: %#v", r)
	}

	// prefix=boo/
	r = get("boo/", "/", "", 1)
	if !r.IsTruncated || !eqSlice(keysOfV2(r), []string{"boo/bar"}) || len(prefsOfV2(r)) != 0 || r.NextContinuationToken == "" {
		t.Fatalf("v2 boo p1: %#v", r)
	}
	r = get("boo/", "/", r.NextContinuationToken, 1)
	if r.IsTruncated || len(keysOfV2(r)) != 0 || !eqSlice(prefsOfV2(r), []string{"boo/baz/"}) || r.NextContinuationToken != "" {
		t.Fatalf("v2 boo p2: %#v", r)
	}
}

func itoa(n int) string {
	// avoid strconv import collision in this file (already imported elsewhere)
	const digits = "0123456789"
	if n == 0 {
		return "0"
	}
	var b strings.Builder
	if n < 0 {
		b.WriteByte('-')
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = digits[n%10]
		n /= 10
	}
	b.Write(buf[i:])
	return b.String()
}
