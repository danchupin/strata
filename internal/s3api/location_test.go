package s3api_test

import (
	"net/http"
	"strings"
	"testing"
)

func TestCreateBucketWithLocationConstraint(t *testing.T) {
	h := newHarness(t)

	body := `<CreateBucketConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
		<LocationConstraint>us-west-2</LocationConstraint>
	</CreateBucketConfiguration>`
	resp := h.doString(http.MethodPut, "/bkt", body)
	h.mustStatus(resp, http.StatusOK)

	resp = h.doString(http.MethodGet, "/bkt?location=", "")
	h.mustStatus(resp, http.StatusOK)
	got := h.readBody(resp)
	if !strings.Contains(got, "<LocationConstraint") || !strings.Contains(got, "us-west-2") {
		t.Fatalf("expected LocationConstraint with us-west-2, got %s", got)
	}
	if !strings.Contains(got, `xmlns="http://s3.amazonaws.com/doc/2006-03-01/"`) {
		t.Fatalf("expected canonical xmlns, got %s", got)
	}
}

func TestCreateBucketDefaultRegion(t *testing.T) {
	h := newHarness(t)

	resp := h.doString(http.MethodPut, "/bkt", "")
	h.mustStatus(resp, http.StatusOK)

	resp = h.doString(http.MethodGet, "/bkt?location=", "")
	h.mustStatus(resp, http.StatusOK)
	got := h.readBody(resp)
	if !strings.Contains(got, "<LocationConstraint") {
		t.Fatalf("expected LocationConstraint element, got %s", got)
	}
	if strings.Contains(got, "us-west-2") {
		t.Fatalf("did not expect a region, got %s", got)
	}
}

func TestHeadBucketRegionHeader(t *testing.T) {
	h := newHarness(t)

	body := `<CreateBucketConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
		<LocationConstraint>eu-central-1</LocationConstraint>
	</CreateBucketConfiguration>`
	resp := h.doString(http.MethodPut, "/bkt", body)
	h.mustStatus(resp, http.StatusOK)

	resp = h.do(http.MethodHead, "/bkt", nil)
	h.mustStatus(resp, http.StatusOK)
	if got := resp.Header.Get("x-amz-bucket-region"); got != "eu-central-1" {
		t.Fatalf("x-amz-bucket-region: got %q want %q", got, "eu-central-1")
	}

	resp = h.doString(http.MethodPut, "/bkt2", "")
	h.mustStatus(resp, http.StatusOK)
	resp = h.do(http.MethodHead, "/bkt2", nil)
	h.mustStatus(resp, http.StatusOK)
	if got := resp.Header.Get("x-amz-bucket-region"); got != "default" {
		t.Fatalf("x-amz-bucket-region default: got %q want %q", got, "default")
	}
}

func TestCreateBucketMalformedLocationBody(t *testing.T) {
	h := newHarness(t)

	resp := h.doString(http.MethodPut, "/bkt", "<not-xml")
	h.mustStatus(resp, http.StatusBadRequest)
}

func TestGetBucketLocationMissing(t *testing.T) {
	h := newHarness(t)
	resp := h.doString(http.MethodGet, "/missing?location=", "")
	h.mustStatus(resp, http.StatusNotFound)
}
