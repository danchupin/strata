package auth

import (
	"net/http"
	"strings"
	"testing"
)

// Vectors from the AWS SigV4 signature reference:
// https://docs.aws.amazon.com/IAM/latest/UserGuide/signing_aws_api_requests.html
const (
	vectorAccessKey = "AKIDEXAMPLE"
	vectorSecret    = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
	vectorRegion    = "us-east-1"
	vectorService   = "service"
	vectorDate      = "20150830T123600Z"
	vectorDay       = "20150830"
)

// TestFullSignatureGetVanilla uses AWS's canonical "get-vanilla" SigV4 test
// vector to verify end-to-end: canonical request → signing key → signature.
// Published expected signature is at
// https://docs.aws.amazon.com/IAM/latest/UserGuide/signing_aws_api_requests.html
func TestFullSignatureGetVanilla(t *testing.T) {
	req, _ := http.NewRequest("GET", "https://example.amazonaws.com/", nil)
	req.Header.Set("X-Amz-Date", vectorDate)
	req.Host = "example.amazonaws.com"

	parsed := &parsedAuth{
		AccessKey:     vectorAccessKey,
		Date:          vectorDay,
		Region:        vectorRegion,
		Service:       vectorService,
		SignedHeaders: []string{"host", "x-amz-date"},
	}
	canonical := canonicalRequest(req, parsed.SignedHeaders, emptyBodyHash)
	signature := computeSignature(vectorSecret, parsed, vectorDate, canonical)

	const expected = "5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31"
	if signature != expected {
		t.Errorf("signature:\n  got  %s\n  want %s", signature, expected)
	}
}

func TestDeriveSigningKeyDeterministic(t *testing.T) {
	a := deriveSigningKey("s3cret", "20260101", "us-east-1", "s3")
	b := deriveSigningKey("s3cret", "20260101", "us-east-1", "s3")
	if string(a) != string(b) {
		t.Error("not deterministic")
	}
	c := deriveSigningKey("s3cret", "20260102", "us-east-1", "s3")
	if string(a) == string(c) {
		t.Error("date did not influence key")
	}
}

func TestCanonicalRequestGetVanilla(t *testing.T) {
	req, _ := http.NewRequest("GET", "https://example.amazonaws.com/", nil)
	req.Header.Set("X-Amz-Date", vectorDate)
	req.Host = "example.amazonaws.com"

	canonical := canonicalRequest(req, []string{"host", "x-amz-date"}, emptyBodyHash)
	want := strings.Join([]string{
		"GET",
		"/",
		"",
		"host:example.amazonaws.com",
		"x-amz-date:" + vectorDate,
		"",
		"host;x-amz-date",
		emptyBodyHash,
	}, "\n")
	if canonical != want {
		t.Fatalf("canonical mismatch:\ngot:\n%s\nwant:\n%s", canonical, want)
	}
}

func TestCanonicalQuerySortAndEncode(t *testing.T) {
	cases := []struct {
		url  string
		want string
	}{
		{"https://x.com/?b=2&a=1", "a=1&b=2"},
		{"https://x.com/?list-type=2&prefix=logs%2F", "list-type=2&prefix=logs%2F"},
		{"https://x.com/?a=b&a=a", "a=a&a=b"},
	}
	for _, tc := range cases {
		req, _ := http.NewRequest("GET", tc.url, nil)
		got := canonicalQuery(req.URL)
		if got != tc.want {
			t.Errorf("%s: got %q want %q", tc.url, got, tc.want)
		}
	}
}

func TestURIEncodePreservesSlashInPath(t *testing.T) {
	got := uriEncode("logs/2026/04/a.log", false)
	want := "logs/2026/04/a.log"
	if got != want {
		t.Errorf("path: got %q want %q", got, want)
	}
	got = uriEncode("logs/2026/04/a.log", true)
	want = "logs%2F2026%2F04%2Fa.log"
	if got != want {
		t.Errorf("query: got %q want %q", got, want)
	}
}

func TestTrimHeaderValue(t *testing.T) {
	cases := []struct{ in, want string }{
		{"  foo  ", "foo"},
		{"foo  bar   baz", "foo bar baz"},
		{"\tfoo\t", "foo"},
	}
	for _, tc := range cases {
		if got := trimHeaderValue(tc.in); got != tc.want {
			t.Errorf("%q: got %q want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseAuthHeader(t *testing.T) {
	h := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=host;x-amz-date, Signature=deadbeef"
	p, err := parseAuthHeader(h)
	if err != nil {
		t.Fatal(err)
	}
	if p.AccessKey != "AKIDEXAMPLE" || p.Date != "20150830" || p.Region != "us-east-1" ||
		p.Service != "service" || p.Signature != "deadbeef" ||
		strings.Join(p.SignedHeaders, ";") != "host;x-amz-date" {
		t.Errorf("parsed wrong: %+v", p)
	}
}

func TestParseAuthHeaderMalformed(t *testing.T) {
	for _, bad := range []string{
		"",
		"AWS4-HMAC-SHA256",
		"AWS4-HMAC-SHA256 Credential=foo, Signature=bar",
		"AWS4-HMAC-SHA256 Credential=a/b/c/d/wrong, SignedHeaders=host, Signature=x",
	} {
		if _, err := parseAuthHeader(bad); err == nil {
			t.Errorf("expected error on %q", bad)
		}
	}
}
