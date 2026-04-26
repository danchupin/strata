package s3api_test

import (
	"strings"
	"testing"
)

const loggingEnabledXML = `<BucketLoggingStatus xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
	<LoggingEnabled>
		<TargetBucket>logs-bucket</TargetBucket>
		<TargetPrefix>access/</TargetPrefix>
	</LoggingEnabled>
</BucketLoggingStatus>`

func TestBucketLoggingEmptyOnFresh(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	resp := h.doString("GET", "/bkt?logging=", "")
	h.mustStatus(resp, 200)
	body := h.readBody(resp)
	if !strings.Contains(body, "<BucketLoggingStatus") {
		t.Fatalf("expected BucketLoggingStatus envelope, got: %s", body)
	}
	if strings.Contains(body, "<LoggingEnabled") {
		t.Fatalf("expected empty BucketLoggingStatus, got: %s", body)
	}
}

func TestBucketLoggingRoundTrip(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt?logging=", loggingEnabledXML), 200)

	resp := h.doString("GET", "/bkt?logging=", "")
	h.mustStatus(resp, 200)
	body := h.readBody(resp)
	if !strings.Contains(body, "<TargetBucket>logs-bucket</TargetBucket>") {
		t.Fatalf("GET logging missing TargetBucket: %s", body)
	}
	if !strings.Contains(body, "<TargetPrefix>access/</TargetPrefix>") {
		t.Fatalf("GET logging missing TargetPrefix: %s", body)
	}
}

func TestBucketLoggingClearOnEmptyBody(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt?logging=", loggingEnabledXML), 200)

	// Empty body PUT clears the configuration.
	h.mustStatus(h.doString("PUT", "/bkt?logging=", ""), 200)

	resp := h.doString("GET", "/bkt?logging=", "")
	h.mustStatus(resp, 200)
	body := h.readBody(resp)
	if !strings.Contains(body, "<BucketLoggingStatus") {
		t.Fatalf("expected BucketLoggingStatus after clear, got: %s", body)
	}
	if strings.Contains(body, "<LoggingEnabled") {
		t.Fatalf("expected no LoggingEnabled after clear, got: %s", body)
	}
}

func TestBucketLoggingMissingLoggingEnabledClears(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt?logging=", loggingEnabledXML), 200)

	// PUT a BucketLoggingStatus without LoggingEnabled also clears.
	h.mustStatus(h.doString("PUT", "/bkt?logging=",
		`<BucketLoggingStatus xmlns="http://s3.amazonaws.com/doc/2006-03-01/"></BucketLoggingStatus>`), 200)

	resp := h.doString("GET", "/bkt?logging=", "")
	h.mustStatus(resp, 200)
	if strings.Contains(h.readBody(resp), "<LoggingEnabled") {
		t.Fatalf("expected no LoggingEnabled after clear via empty status")
	}
}

func TestBucketLoggingMalformedBody(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	resp := h.doString("PUT", "/bkt?logging=", "<BucketLoggingStatus><nope")
	h.mustStatus(resp, 400)
	if body := h.readBody(resp); !strings.Contains(body, "MalformedXML") {
		t.Fatalf("expected MalformedXML, got: %s", body)
	}
}

func TestBucketLoggingMissingTargetBucket(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	resp := h.doString("PUT", "/bkt?logging=",
		`<BucketLoggingStatus><LoggingEnabled><TargetPrefix>x/</TargetPrefix></LoggingEnabled></BucketLoggingStatus>`)
	h.mustStatus(resp, 400)
	if body := h.readBody(resp); !strings.Contains(body, "MalformedXML") {
		t.Fatalf("expected MalformedXML, got: %s", body)
	}
}

func TestBucketLoggingOnMissingBucket(t *testing.T) {
	h := newHarness(t)
	resp := h.doString("GET", "/missing?logging=", "")
	h.mustStatus(resp, 404)
}
