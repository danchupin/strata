package s3api_test

import (
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
