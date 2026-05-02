package s3api_test

import (
	"net/http"
	"strings"
	"testing"
)

const websiteBasicXML = `<WebsiteConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
	<IndexDocument><Suffix>index.html</Suffix></IndexDocument>
	<ErrorDocument><Key>error.html</Key></ErrorDocument>
</WebsiteConfiguration>`

const websiteIndexOnlyXML = `<WebsiteConfiguration>
	<IndexDocument><Suffix>index.html</Suffix></IndexDocument>
</WebsiteConfiguration>`

func TestBucketWebsiteCRUD(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	// GET on fresh bucket → 404 NoSuchWebsiteConfiguration.
	resp := h.doString("GET", "/bkt?website=", "")
	h.mustStatus(resp, 404)
	if body := h.readBody(resp); !strings.Contains(body, "NoSuchWebsiteConfiguration") {
		t.Fatalf("expected NoSuchWebsiteConfiguration, got: %s", body)
	}

	// PUT round-trips.
	h.mustStatus(h.doString("PUT", "/bkt?website=", websiteBasicXML), 200)
	resp = h.doString("GET", "/bkt?website=", "")
	h.mustStatus(resp, 200)
	body := h.readBody(resp)
	if !strings.Contains(body, "IndexDocument") || !strings.Contains(body, "index.html") {
		t.Fatalf("GET website body missing IndexDocument: %s", body)
	}
	if !strings.Contains(body, "error.html") {
		t.Fatalf("GET website body missing ErrorDocument: %s", body)
	}

	// DELETE clears.
	h.mustStatus(h.doString("DELETE", "/bkt?website=", ""), 204)
	resp = h.doString("GET", "/bkt?website=", "")
	h.mustStatus(resp, 404)
}

func TestBucketWebsiteMalformedRejected(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	resp := h.doString("PUT", "/bkt?website=", "<WebsiteConfiguration><nope")
	h.mustStatus(resp, 400)
	if body := h.readBody(resp); !strings.Contains(body, "MalformedXML") {
		t.Fatalf("expected MalformedXML, got: %s", body)
	}

	// IndexDocument required when no RedirectAllRequestsTo.
	resp = h.doString("PUT", "/bkt?website=",
		"<WebsiteConfiguration></WebsiteConfiguration>")
	h.mustStatus(resp, 400)
	if body := h.readBody(resp); !strings.Contains(body, "InvalidArgument") {
		t.Fatalf("expected InvalidArgument, got: %s", body)
	}

	// IndexDocument suffix may not contain '/'.
	resp = h.doString("PUT", "/bkt?website=",
		`<WebsiteConfiguration><IndexDocument><Suffix>a/b.html</Suffix></IndexDocument></WebsiteConfiguration>`)
	h.mustStatus(resp, 400)
	if body := h.readBody(resp); !strings.Contains(body, "InvalidArgument") {
		t.Fatalf("expected InvalidArgument, got: %s", body)
	}
}

func TestBucketWebsiteIndexServing(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt?website=", websiteBasicXML), 200)

	// Upload the index document.
	h.mustStatus(h.doString("PUT", "/bkt/index.html", "<html>Welcome</html>",
		"Content-Type", "text/html"), 200)

	// GET / serves the index.
	resp := h.doString("GET", "/bkt/", "")
	h.mustStatus(resp, 200)
	body := h.readBody(resp)
	if !strings.Contains(body, "Welcome") {
		t.Fatalf("GET /bkt/ did not serve index.html, got: %s", body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/html" {
		t.Fatalf("expected Content-Type text/html, got %q", ct)
	}
}

func TestBucketWebsiteErrorDocServing(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt?website=", websiteBasicXML), 200)

	// Upload only the error document; no index.
	h.mustStatus(h.doString("PUT", "/bkt/error.html", "<html>Not Found</html>",
		"Content-Type", "text/html"), 200)

	// GET / falls through to error doc with 404.
	resp := h.doString("GET", "/bkt/", "")
	h.mustStatus(resp, 404)
	body := h.readBody(resp)
	if !strings.Contains(body, "Not Found") {
		t.Fatalf("expected error doc body, got: %s", body)
	}
}

func TestBucketWebsiteNoIndexNoError(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt?website=", websiteIndexOnlyXML), 200)

	// No index uploaded, no error doc configured → NoSuchKey.
	resp := h.doString("GET", "/bkt/", "")
	h.mustStatus(resp, 404)
	if body := h.readBody(resp); !strings.Contains(body, "NoSuchKey") {
		t.Fatalf("expected NoSuchKey, got: %s", body)
	}
}

func TestBucketWebsiteNotConfiguredFallsThroughToList(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt/foo", "data"), 200)

	// No website config → GET /bkt/ behaves as ListObjects.
	resp := h.doString("GET", "/bkt/", "")
	h.mustStatus(resp, 200)
	body := h.readBody(resp)
	if !strings.Contains(body, "ListBucketResult") || !strings.Contains(body, "foo") {
		t.Fatalf("expected ListBucketResult containing foo, got: %s", body)
	}
}

func TestBucketWebsiteOnMissingBucket(t *testing.T) {
	h := newHarness(t)
	resp := h.doString("GET", "/missing?website=", "")
	h.mustStatus(resp, 404)
}

const websiteRedirectAllXML = `<WebsiteConfiguration>
	<RedirectAllRequestsTo>
		<HostName>example.com</HostName>
		<Protocol>https</Protocol>
	</RedirectAllRequestsTo>
</WebsiteConfiguration>`

const websiteRedirectAllNoProtoXML = `<WebsiteConfiguration>
	<RedirectAllRequestsTo>
		<HostName>example.com</HostName>
	</RedirectAllRequestsTo>
</WebsiteConfiguration>`

// noRedirectClient returns an http.Client that does not follow redirects so
// tests can assert on 301 responses directly.
func noRedirectClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func (h *testHarness) doNoRedirect(method, path string) *http.Response {
	h.t.Helper()
	req, err := http.NewRequest(method, h.ts.URL+path, nil)
	if err != nil {
		h.t.Fatalf("new request: %v", err)
	}
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		h.t.Fatalf("request %s %s: %v", method, path, err)
	}
	return resp
}

func TestBucketWebsiteRedirectAllRequestsTo(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt?website=", websiteRedirectAllXML), 200)

	// GET on bucket root returns 301 to https://example.com.
	resp := h.doNoRedirect("GET", "/bkt/")
	h.mustStatus(resp, 301)
	if loc := resp.Header.Get("Location"); loc != "https://example.com" {
		t.Fatalf("root Location: got %q want %q", loc, "https://example.com")
	}
	_ = h.readBody(resp)

	// GET on object path returns 301 with key appended.
	resp = h.doNoRedirect("GET", "/bkt/some/key.html")
	h.mustStatus(resp, 301)
	if loc := resp.Header.Get("Location"); loc != "https://example.com/some/key.html" {
		t.Fatalf("key Location: got %q want %q", loc, "https://example.com/some/key.html")
	}
	_ = h.readBody(resp)
}

func TestBucketWebsiteRedirectAllDefaultsToHTTP(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt?website=", websiteRedirectAllNoProtoXML), 200)

	resp := h.doNoRedirect("GET", "/bkt/anything")
	h.mustStatus(resp, 301)
	if loc := resp.Header.Get("Location"); loc != "http://example.com/anything" {
		t.Fatalf("Location: got %q want %q", loc, "http://example.com/anything")
	}
	_ = h.readBody(resp)
}

func TestBucketWebsiteRedirectAllOnlyAffectsGET(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt?website=", websiteRedirectAllXML), 200)

	// PUT continues normally — object lands without redirect.
	h.mustStatus(h.doString("PUT", "/bkt/foo", "data"), 200)

	// HEAD on existing object continues normally — 200, not 301.
	resp := h.doNoRedirect("HEAD", "/bkt/foo")
	if resp.StatusCode == http.StatusMovedPermanently {
		t.Fatalf("HEAD got 301; expected normal HEAD response")
	}
	_ = resp.Body.Close()

	// DELETE continues normally.
	h.mustStatus(h.doString("DELETE", "/bkt/foo", ""), 204)
}

const websiteRoutingPrefixXML = `<WebsiteConfiguration>
	<IndexDocument><Suffix>index.html</Suffix></IndexDocument>
	<RoutingRules>
		<RoutingRule>
			<Condition><KeyPrefixEquals>docs/</KeyPrefixEquals></Condition>
			<Redirect>
				<HostName>example.com</HostName>
				<Protocol>https</Protocol>
				<ReplaceKeyPrefixWith>documents/</ReplaceKeyPrefixWith>
			</Redirect>
		</RoutingRule>
	</RoutingRules>
</WebsiteConfiguration>`

const websiteRoutingErrorXML = `<WebsiteConfiguration>
	<IndexDocument><Suffix>index.html</Suffix></IndexDocument>
	<RoutingRules>
		<RoutingRule>
			<Condition><HttpErrorCodeReturnedEquals>404</HttpErrorCodeReturnedEquals></Condition>
			<Redirect>
				<HostName>example.com</HostName>
				<Protocol>https</Protocol>
				<ReplaceKeyWith>404.html</ReplaceKeyWith>
			</Redirect>
		</RoutingRule>
	</RoutingRules>
</WebsiteConfiguration>`

func TestBucketWebsiteRoutingRulePrefix(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt?website=", websiteRoutingPrefixXML), 200)

	resp := h.doNoRedirect("GET", "/bkt/docs/foo.html")
	h.mustStatus(resp, 301)
	if loc := resp.Header.Get("Location"); loc != "https://example.com/documents/foo.html" {
		t.Fatalf("Location: got %q want %q", loc, "https://example.com/documents/foo.html")
	}
	_ = h.readBody(resp)

	// Non-matching prefix falls through to normal lookup.
	resp = h.doNoRedirect("GET", "/bkt/other/path.html")
	if resp.StatusCode == http.StatusMovedPermanently {
		t.Fatalf("non-matching prefix should not redirect")
	}
	_ = resp.Body.Close()
}

func TestBucketWebsiteRoutingRuleErrorCode(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt?website=", websiteRoutingErrorXML), 200)

	// Missing object → 301 to /404.html.
	resp := h.doNoRedirect("GET", "/bkt/missing-key")
	h.mustStatus(resp, 301)
	if loc := resp.Header.Get("Location"); loc != "https://example.com/404.html" {
		t.Fatalf("Location: got %q want %q", loc, "https://example.com/404.html")
	}
	_ = h.readBody(resp)

	// Existing object served normally — no redirect.
	h.mustStatus(h.doString("PUT", "/bkt/exists", "data"), 200)
	resp = h.doNoRedirect("GET", "/bkt/exists")
	if resp.StatusCode == http.StatusMovedPermanently {
		t.Fatalf("existing object should not redirect")
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if body := h.readBody(resp); body != "data" {
		t.Fatalf("body: got %q want %q", body, "data")
	}
}

func TestBucketWebsiteRoutingRulePreservesIndex(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt?website=", websiteRoutingPrefixXML), 200)

	// Upload index doc.
	h.mustStatus(h.doString("PUT", "/bkt/index.html", "<html>Welcome</html>",
		"Content-Type", "text/html"), 200)

	// GET / serves index — routing rule for "docs/" prefix doesn't fire.
	resp := h.doNoRedirect("GET", "/bkt/")
	h.mustStatus(resp, 200)
	body := h.readBody(resp)
	if !strings.Contains(body, "Welcome") {
		t.Fatalf("expected index body, got: %s", body)
	}
}

func TestBucketWebsiteRoutingRuleRejectsInvalid(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	// Rule with empty Redirect (no HostName, no ReplaceKey*) → 400.
	bad := `<WebsiteConfiguration>
		<IndexDocument><Suffix>index.html</Suffix></IndexDocument>
		<RoutingRules>
			<RoutingRule>
				<Condition><KeyPrefixEquals>x/</KeyPrefixEquals></Condition>
				<Redirect></Redirect>
			</RoutingRule>
		</RoutingRules>
	</WebsiteConfiguration>`
	resp := h.doString("PUT", "/bkt?website=", bad)
	h.mustStatus(resp, 400)
	if body := h.readBody(resp); !strings.Contains(body, "InvalidArgument") {
		t.Fatalf("expected InvalidArgument, got: %s", body)
	}
}

func TestBucketWebsiteNoRedirectWithoutConfig(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt/foo", "data"), 200)

	// No website config → GET returns object body, not redirect.
	resp := h.doNoRedirect("GET", "/bkt/foo")
	if resp.StatusCode == http.StatusMovedPermanently {
		t.Fatalf("expected no redirect without website config; got 301")
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if body := h.readBody(resp); body != "data" {
		t.Fatalf("body: got %q want %q", body, "data")
	}
}
