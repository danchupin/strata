package s3api_test

import (
	"fmt"
	"strings"
	"testing"
)

const bucketTaggingXML = `<Tagging xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
	<TagSet>
		<Tag><Key>team</Key><Value>storage</Value></Tag>
		<Tag><Key>env</Key><Value>prod</Value></Tag>
	</TagSet>
</Tagging>`

func TestBucketTaggingRoundTrip(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt?tagging=", bucketTaggingXML), 204)

	resp := h.doString("GET", "/bkt?tagging=", "")
	h.mustStatus(resp, 200)
	body := h.readBody(resp)
	if !strings.Contains(body, "<Key>team</Key>") || !strings.Contains(body, "<Value>storage</Value>") {
		t.Fatalf("missing team tag: %s", body)
	}
	if !strings.Contains(body, "<Key>env</Key>") || !strings.Contains(body, "<Value>prod</Value>") {
		t.Fatalf("missing env tag: %s", body)
	}
}

func TestBucketTaggingGetMissing(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	resp := h.doString("GET", "/bkt?tagging=", "")
	h.mustStatus(resp, 404)
	if body := h.readBody(resp); !strings.Contains(body, "NoSuchTagSet") {
		t.Fatalf("expected NoSuchTagSet, got: %s", body)
	}
}

func TestBucketTaggingDelete(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt?tagging=", bucketTaggingXML), 204)
	h.mustStatus(h.doString("DELETE", "/bkt?tagging=", ""), 204)

	resp := h.doString("GET", "/bkt?tagging=", "")
	h.mustStatus(resp, 404)
}

func TestBucketTaggingPutEmptyClears(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt?tagging=", bucketTaggingXML), 204)
	h.mustStatus(h.doString("PUT", "/bkt?tagging=", ""), 204)

	resp := h.doString("GET", "/bkt?tagging=", "")
	h.mustStatus(resp, 404)
}

func TestBucketTaggingOverLimit(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	var b strings.Builder
	b.WriteString(`<Tagging><TagSet>`)
	for i := range 51 {
		fmt.Fprintf(&b, `<Tag><Key>k%d</Key><Value>v%d</Value></Tag>`, i, i)
	}
	b.WriteString(`</TagSet></Tagging>`)

	resp := h.doString("PUT", "/bkt?tagging=", b.String())
	h.mustStatus(resp, 400)
	if body := h.readBody(resp); !strings.Contains(body, "InvalidTag") {
		t.Fatalf("expected InvalidTag, got: %s", body)
	}
}

func TestBucketTaggingMalformedBody(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	resp := h.doString("PUT", "/bkt?tagging=", "<Tagging><nope")
	h.mustStatus(resp, 400)
	if body := h.readBody(resp); !strings.Contains(body, "MalformedXML") {
		t.Fatalf("expected MalformedXML, got: %s", body)
	}
}

func TestBucketTaggingOnMissingBucket(t *testing.T) {
	h := newHarness(t)
	resp := h.doString("GET", "/missing?tagging=", "")
	h.mustStatus(resp, 404)
	if body := h.readBody(resp); !strings.Contains(body, "NoSuchBucket") {
		t.Fatalf("expected NoSuchBucket, got: %s", body)
	}
}

func TestBucketTaggingDuplicateKey(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	resp := h.doString("PUT", "/bkt?tagging=",
		`<Tagging><TagSet><Tag><Key>a</Key><Value>1</Value></Tag><Tag><Key>a</Key><Value>2</Value></Tag></TagSet></Tagging>`)
	h.mustStatus(resp, 400)
	if body := h.readBody(resp); !strings.Contains(body, "InvalidTag") {
		t.Fatalf("expected InvalidTag, got: %s", body)
	}
}

func TestBucketTaggingEmptyKeyRejected(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	resp := h.doString("PUT", "/bkt?tagging=",
		`<Tagging><TagSet><Tag><Key></Key><Value>v</Value></Tag></TagSet></Tagging>`)
	h.mustStatus(resp, 400)
	if body := h.readBody(resp); !strings.Contains(body, "InvalidTag") {
		t.Fatalf("expected InvalidTag, got: %s", body)
	}
}
