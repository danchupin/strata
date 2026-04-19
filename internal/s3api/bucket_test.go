package s3api_test

import (
	"strings"
	"testing"
)

func TestBucketCRUD(t *testing.T) {
	h := newHarness(t)

	h.mustStatus(h.doString("PUT", "/photos", ""), 200)
	h.mustStatus(h.doString("HEAD", "/photos", ""), 200)
	h.mustStatus(h.doString("HEAD", "/nonexistent", ""), 404)

	resp := h.doString("GET", "/", "")
	h.mustStatus(resp, 200)
	body := h.readBody(resp)
	if !strings.Contains(body, "<Name>photos</Name>") {
		t.Errorf("ListBuckets response missing photos: %s", body)
	}

	h.mustStatus(h.doString("PUT", "/photos", ""), 409)
	h.mustStatus(h.doString("DELETE", "/photos", ""), 204)
	h.mustStatus(h.doString("HEAD", "/photos", ""), 404)
}

func TestBucketDeleteRejectsNonEmpty(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/b", ""), 200)
	h.mustStatus(h.doString("PUT", "/b/key.txt", "x"), 200)

	resp := h.doString("DELETE", "/b", "")
	h.mustStatus(resp, 409)
	body := h.readBody(resp)
	if !strings.Contains(body, "BucketNotEmpty") {
		t.Errorf("expected BucketNotEmpty, got: %s", body)
	}

	h.mustStatus(h.doString("DELETE", "/b/key.txt", ""), 204)
	h.mustStatus(h.doString("DELETE", "/b", ""), 204)
}
