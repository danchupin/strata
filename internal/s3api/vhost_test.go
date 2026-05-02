package s3api_test

import (
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/danchupin/strata/internal/auth"
	datamem "github.com/danchupin/strata/internal/data/memory"
	metamem "github.com/danchupin/strata/internal/meta/memory"
	"github.com/danchupin/strata/internal/s3api"
)

// vhostHarness is a thin clone of testHarness that exposes VHostPatterns and
// lets each request override the Host header — the wire identity needed for
// virtual-hosted-style routing.
type vhostHarness struct {
	t  *testing.T
	ts *httptest.Server
}

func newVHostHarness(t *testing.T, patterns []string) *vhostHarness {
	t.Helper()
	api := s3api.New(datamem.New(), metamem.New())
	api.Region = "default"
	api.Master = harnessMasterProvider{}
	api.VHostPatterns = patterns
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p := r.Header.Get(testPrincipalHeader); p != "" {
			ctx := auth.WithAuth(r.Context(), &auth.AuthInfo{Owner: p, AccessKey: p})
			r = r.WithContext(ctx)
		}
		api.ServeHTTP(w, r)
	})
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return &vhostHarness{t: t, ts: ts}
}

func (h *vhostHarness) do(method, path, body, host string, headers ...string) *http.Response {
	h.t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, h.ts.URL+path, r)
	if err != nil {
		h.t.Fatalf("new request: %v", err)
	}
	if host != "" {
		req.Host = host
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("request %s %s: %v", method, path, err)
	}
	return resp
}

func vhostMustStatus(t *testing.T, resp *http.Response, want int) {
	t.Helper()
	if resp.StatusCode != want {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Fatalf("status: got %d want %d; body=%s", resp.StatusCode, want, string(body))
	}
}

func TestVHostStyle_RoundTripPUTGetHead(t *testing.T) {
	h := newVHostHarness(t, []string{"*.s3.local"})

	vhostMustStatus(t, h.do("PUT", "/", "", "bkt.s3.local"), 200)

	body := "hello vhost"
	vhostMustStatus(t, h.do("PUT", "/key.txt", body, "bkt.s3.local"), 200)

	resp := h.do("GET", "/key.txt", "", "bkt.s3.local")
	vhostMustStatus(t, resp, 200)
	got, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(got) != body {
		t.Fatalf("body mismatch: got %q want %q", string(got), body)
	}

	resp = h.do("HEAD", "/key.txt", "", "bkt.s3.local")
	vhostMustStatus(t, resp, 200)
	_ = resp.Body.Close()
}

func TestVHostStyle_PathStyleStillWorks(t *testing.T) {
	h := newVHostHarness(t, []string{"*.s3.local"})

	vhostMustStatus(t, h.do("PUT", "/bkt", "", ""), 200)
	vhostMustStatus(t, h.do("PUT", "/bkt/key.txt", "abc", ""), 200)

	resp := h.do("GET", "/bkt/key.txt", "", "")
	vhostMustStatus(t, resp, 200)
	got, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(got) != "abc" {
		t.Fatalf("body mismatch: got %q", string(got))
	}
}

func TestVHostStyle_MismatchedHostFallsBack(t *testing.T) {
	h := newVHostHarness(t, []string{"*.s3.local"})

	// Host doesn't match the configured pattern, so the request stays
	// path-style and the bucket comes from /bkt.
	vhostMustStatus(t, h.do("PUT", "/bkt", "", "bkt.s3.example.com"), 200)
	vhostMustStatus(t, h.do("PUT", "/bkt/key.txt", "xyz", "bkt.s3.example.com"), 200)
	resp := h.do("GET", "/bkt/key.txt", "", "bkt.s3.example.com")
	vhostMustStatus(t, resp, 200)
	_ = resp.Body.Close()
}

func TestVHostStyle_DisabledByDefault(t *testing.T) {
	h := newVHostHarness(t, nil)

	// Without patterns we never strip the bucket prefix from Host.
	// Request below is virtual-hosted-shaped (bucket in Host, key in path);
	// because vhost is disabled, the gateway sees no bucket -> ListBuckets.
	resp := h.do("GET", "/", "", "bkt.s3.local", "X-Test-Principal", "owner")
	vhostMustStatus(t, resp, 200)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "ListAllMyBucketsResult") {
		t.Fatalf("expected ListBuckets response, got: %s", string(body))
	}
}

func TestVHostStyle_SignedRoundTrip(t *testing.T) {
	ms := metamem.New()
	api := s3api.New(datamem.New(), ms)
	api.Region = "us-east-1"
	api.VHostPatterns = []string{"*.s3.local"}

	stores := []auth.CredentialsStore{
		auth.NewStaticStore(map[string]*auth.Credential{
			iamRootAK: {AccessKey: iamRootAK, Secret: iamRootSK, Owner: s3api.IAMRootPrincipal},
		}),
		metamem.NewCredentialStore(ms),
	}
	multi := auth.NewMultiStore(time.Minute, stores...)
	mw := &auth.Middleware{Store: multi, Mode: auth.ModeRequired}
	ts := httptest.NewServer(mw.Wrap(api, s3api.WriteAuthDenied))
	t.Cleanup(ts.Close)

	// PUT bucket via vhost: signed Host = bkt.s3.local, URL = ts.URL/.
	putReq, err := http.NewRequest("PUT", ts.URL+"/", nil)
	if err != nil {
		t.Fatalf("new req: %v", err)
	}
	putReq.Host = "bkt.s3.local"
	putReq.Header.Set("Host", "bkt.s3.local")
	signRequest(t, putReq, iamRootAK, iamRootSK, "us-east-1")
	resp, err := http.DefaultClient.Do(putReq)
	if err != nil {
		t.Fatalf("put bucket: %v", err)
	}
	vhostMustStatus(t, resp, 200)
	_ = resp.Body.Close()

	// PUT object via vhost (path = /key.txt, Host = bkt.s3.local).
	body := "signed vhost"
	objReq, err := http.NewRequest("PUT", ts.URL+"/key.txt", strings.NewReader(body))
	if err != nil {
		t.Fatalf("new req: %v", err)
	}
	objReq.Host = "bkt.s3.local"
	objReq.Header.Set("Host", "bkt.s3.local")
	objReq.ContentLength = int64(len(body))
	signRequest(t, objReq, iamRootAK, iamRootSK, "us-east-1")
	resp, err = http.DefaultClient.Do(objReq)
	if err != nil {
		t.Fatalf("put obj: %v", err)
	}
	vhostMustStatus(t, resp, 200)
	_ = resp.Body.Close()

	// GET it back via vhost.
	getReq, err := http.NewRequest("GET", ts.URL+"/key.txt", nil)
	if err != nil {
		t.Fatalf("new req: %v", err)
	}
	getReq.Host = "bkt.s3.local"
	getReq.Header.Set("Host", "bkt.s3.local")
	signRequest(t, getReq, iamRootAK, iamRootSK, "us-east-1")
	resp, err = http.DefaultClient.Do(getReq)
	if err != nil {
		t.Fatalf("get obj: %v", err)
	}
	vhostMustStatus(t, resp, 200)
	got, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(got) != body {
		t.Fatalf("body mismatch: got %q", string(got))
	}
}

func TestVHostStyle_PresignedRoundTrip(t *testing.T) {
	ms := metamem.New()
	api := s3api.New(datamem.New(), ms)
	api.Region = "us-east-1"
	api.VHostPatterns = []string{"*.s3.local"}

	stores := []auth.CredentialsStore{
		auth.NewStaticStore(map[string]*auth.Credential{
			iamRootAK: {AccessKey: iamRootAK, Secret: iamRootSK, Owner: s3api.IAMRootPrincipal},
		}),
		metamem.NewCredentialStore(ms),
	}
	multi := auth.NewMultiStore(time.Minute, stores...)
	mw := &auth.Middleware{Store: multi, Mode: auth.ModeRequired}
	ts := httptest.NewServer(mw.Wrap(api, s3api.WriteAuthDenied))
	t.Cleanup(ts.Close)

	// Seed a bucket + object via signed PUT (vhost), then GET via a presigned vhost URL.
	put := func(method, path, body string) {
		t.Helper()
		var r io.Reader
		if body != "" {
			r = strings.NewReader(body)
		}
		req, err := http.NewRequest(method, ts.URL+path, r)
		if err != nil {
			t.Fatalf("new req: %v", err)
		}
		req.Host = "bkt.s3.local"
		req.Header.Set("Host", "bkt.s3.local")
		if body != "" {
			req.ContentLength = int64(len(body))
		}
		signRequest(t, req, iamRootAK, iamRootSK, "us-east-1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		vhostMustStatus(t, resp, 200)
		_ = resp.Body.Close()
	}
	put("PUT", "/", "")
	put("PUT", "/k", "presigned vhost body")

	// Build a presigned GET URL signing host=bkt.s3.local + path=/k.
	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	day := now.Format("20060102")
	scope := day + "/us-east-1/s3/aws4_request"
	q := url.Values{}
	q.Set("X-Amz-Algorithm", "AWS4-HMAC-SHA256")
	q.Set("X-Amz-Credential", iamRootAK+"/"+scope)
	q.Set("X-Amz-Date", amzDate)
	q.Set("X-Amz-Expires", "60")
	q.Set("X-Amz-SignedHeaders", "host")
	canonicalQ := canonicalQuery(&url.URL{RawQuery: q.Encode()})
	canonical := strings.Join([]string{
		"GET",
		canonicalPath("/k"),
		canonicalQ,
		"host:bkt.s3.local\n",
		"host",
		"UNSIGNED-PAYLOAD",
	}, "\n")
	sts := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		sha256Hex([]byte(canonical)),
	}, "\n")
	kDate := hmacSHA256([]byte("AWS4"+iamRootSK), []byte(day))
	kRegion := hmacSHA256(kDate, []byte("us-east-1"))
	kService := hmacSHA256(kRegion, []byte("s3"))
	kSigning := hmacSHA256(kService, []byte("aws4_request"))
	sig := hex.EncodeToString(hmacSHA256(kSigning, []byte(sts)))
	q.Set("X-Amz-Signature", sig)

	getReq, err := http.NewRequest("GET", ts.URL+"/k?"+q.Encode(), nil)
	if err != nil {
		t.Fatalf("new req: %v", err)
	}
	getReq.Host = "bkt.s3.local"
	getReq.Header.Set("Host", "bkt.s3.local")
	resp, err := http.DefaultClient.Do(getReq)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	vhostMustStatus(t, resp, 200)
	got, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(got) != "presigned vhost body" {
		t.Fatalf("body mismatch: got %q", string(got))
	}
}

func TestVHostStyle_PortInHostStripped(t *testing.T) {
	h := newVHostHarness(t, []string{"*.s3.local"})

	vhostMustStatus(t, h.do("PUT", "/", "", "bkt.s3.local:9000"), 200)
	vhostMustStatus(t, h.do("PUT", "/key.txt", "p", "bkt.s3.local:9000"), 200)
	resp := h.do("GET", "/key.txt", "", "bkt.s3.local:9000")
	vhostMustStatus(t, resp, 200)
	_ = resp.Body.Close()
}

func TestParseVHostPatterns(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{" *.s3.local ", []string{"*.s3.local"}},
		{" *.s3.local , *.s3.example.com ", []string{"*.s3.local", "*.s3.example.com"}},
		{",,,", nil},
	}
	for _, c := range cases {
		got := s3api.ParseVHostPatterns(c.in)
		if len(got) != len(c.want) {
			t.Fatalf("in=%q: got %v want %v", c.in, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("in=%q[%d]: got %q want %q", c.in, i, got[i], c.want[i])
			}
		}
	}
}
