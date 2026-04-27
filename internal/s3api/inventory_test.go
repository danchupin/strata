package s3api_test

import (
	"net/http"
	"strings"
	"testing"
)

const sampleInventoryXML = `<?xml version="1.0" encoding="UTF-8"?>
<InventoryConfiguration>
  <Id>list1</Id>
  <IsEnabled>true</IsEnabled>
  <Destination>
    <S3BucketDestination>
      <Bucket>arn:aws:s3:::dest</Bucket>
      <Format>CSV</Format>
      <Prefix>inv/</Prefix>
    </S3BucketDestination>
  </Destination>
  <Schedule>
    <Frequency>Daily</Frequency>
  </Schedule>
  <IncludedObjectVersions>Current</IncludedObjectVersions>
</InventoryConfiguration>`

func TestBucketInventoryConfigCRUD(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString(http.MethodPut, "/bkt", "", testPrincipalHeader, "u"), http.StatusOK)

	// PUT first config
	resp := h.doString(http.MethodPut, "/bkt?inventory&id=list1", sampleInventoryXML, testPrincipalHeader, "u")
	h.mustStatus(resp, http.StatusOK)
	resp.Body.Close()

	// GET round-trip — body matches PUT body
	resp = h.doString(http.MethodGet, "/bkt?inventory&id=list1", "", testPrincipalHeader, "u")
	h.mustStatus(resp, http.StatusOK)
	body := h.readBody(resp)
	if !strings.Contains(body, "<Id>list1</Id>") || !strings.Contains(body, "<Frequency>Daily</Frequency>") {
		t.Fatalf("get round-trip body unexpected: %s", body)
	}

	// LIST surfaces the entry
	resp = h.doString(http.MethodGet, "/bkt?inventory", "", testPrincipalHeader, "u")
	h.mustStatus(resp, http.StatusOK)
	listBody := h.readBody(resp)
	if !strings.Contains(listBody, "list1") || !strings.Contains(listBody, "ListInventoryConfigurationsResult") {
		t.Fatalf("list body: %s", listBody)
	}

	// DELETE
	resp = h.doString(http.MethodDelete, "/bkt?inventory&id=list1", "", testPrincipalHeader, "u")
	h.mustStatus(resp, http.StatusNoContent)
	resp.Body.Close()

	// GET after delete returns 404
	resp = h.doString(http.MethodGet, "/bkt?inventory&id=list1", "", testPrincipalHeader, "u")
	h.mustStatus(resp, http.StatusNotFound)
	resp.Body.Close()
}

func TestBucketInventoryConfigIDMismatchReturns400(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString(http.MethodPut, "/bkt", "", testPrincipalHeader, "u"), http.StatusOK)
	// URL says id=list2 but body says <Id>list1</Id>.
	resp := h.doString(http.MethodPut, "/bkt?inventory&id=list2", sampleInventoryXML, testPrincipalHeader, "u")
	h.mustStatus(resp, http.StatusBadRequest)
	resp.Body.Close()
}

func TestBucketInventoryConfigMalformedRejected(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString(http.MethodPut, "/bkt", "", testPrincipalHeader, "u"), http.StatusOK)

	// Missing Schedule → MalformedXML
	bad := `<InventoryConfiguration><Id>x</Id><IsEnabled>true</IsEnabled>` +
		`<Destination><S3BucketDestination><Bucket>arn:aws:s3:::d</Bucket><Format>CSV</Format></S3BucketDestination></Destination>` +
		`<IncludedObjectVersions>Current</IncludedObjectVersions></InventoryConfiguration>`
	resp := h.doString(http.MethodPut, "/bkt?inventory&id=x", bad, testPrincipalHeader, "u")
	h.mustStatus(resp, http.StatusBadRequest)
	resp.Body.Close()
}

func TestBucketInventoryConfigMultipleIDs(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString(http.MethodPut, "/bkt", "", testPrincipalHeader, "u"), http.StatusOK)

	// Two configs by different IDs, both round-trip independently.
	cfg2 := strings.Replace(sampleInventoryXML, "list1", "list2", 1)
	h.mustStatus(h.doString(http.MethodPut, "/bkt?inventory&id=list1", sampleInventoryXML, testPrincipalHeader, "u"), http.StatusOK)
	h.mustStatus(h.doString(http.MethodPut, "/bkt?inventory&id=list2", cfg2, testPrincipalHeader, "u"), http.StatusOK)

	resp := h.doString(http.MethodGet, "/bkt?inventory", "", testPrincipalHeader, "u")
	body := h.readBody(resp)
	if !strings.Contains(body, "list1") || !strings.Contains(body, "list2") {
		t.Fatalf("list missing ids: %s", body)
	}
}
