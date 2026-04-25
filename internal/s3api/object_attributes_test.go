package s3api_test

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
)

func TestGetObjectAttributesFullResponse(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	payload := strings.Repeat("z", 257)
	digest := sha256.Sum256([]byte(payload))
	wantSHA256 := base64.StdEncoding.EncodeToString(digest[:])
	put := h.doString("PUT", "/bkt/k", payload, "x-amz-checksum-sha256", wantSHA256)
	h.mustStatus(put, 200)
	_ = h.readBody(put)

	resp := h.doString("GET", "/bkt/k?attributes", "",
		"x-amz-object-attributes", "ETag,Checksum,ObjectParts,StorageClass,ObjectSize")
	h.mustStatus(resp, 200)
	body := h.readBody(resp)

	for _, want := range []string{
		"<GetObjectAttributesOutput>",
		"<ETag>",
		"<StorageClass>STANDARD</StorageClass>",
		fmt.Sprintf("<ObjectSize>%d</ObjectSize>", len(payload)),
		"<Checksum>",
		"<ChecksumSHA256>" + wantSHA256 + "</ChecksumSHA256>",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("response missing %q; body=%s", want, body)
		}
	}
	// Single-part PUT has no ObjectParts subtree.
	if strings.Contains(body, "<ObjectParts>") {
		t.Fatalf("did not expect ObjectParts on single-part object; body=%s", body)
	}
}

func TestGetObjectAttributesPartialSubset(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt/k", "hello"), 200)

	resp := h.doString("GET", "/bkt/k?attributes", "",
		"x-amz-object-attributes", "ETag,ObjectSize")
	h.mustStatus(resp, 200)
	body := h.readBody(resp)

	if !strings.Contains(body, "<ETag>") {
		t.Fatalf("expected ETag in body: %s", body)
	}
	if !strings.Contains(body, "<ObjectSize>5</ObjectSize>") {
		t.Fatalf("expected ObjectSize=5 in body: %s", body)
	}
	for _, unwanted := range []string{"<StorageClass>", "<Checksum>", "<ObjectParts>"} {
		if strings.Contains(body, unwanted) {
			t.Fatalf("did not expect %s for partial request; body=%s", unwanted, body)
		}
	}
}

func TestGetObjectAttributesMissingKeyReturns404(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	resp := h.doString("GET", "/bkt/missing?attributes", "",
		"x-amz-object-attributes", "ETag")
	h.mustStatus(resp, 404)
	if !strings.Contains(h.readBody(resp), "NoSuchKey") {
		t.Fatal("expected NoSuchKey body")
	}
}

func TestGetObjectAttributesMissingHeaderReturns400(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt/k", "hello"), 200)

	resp := h.doString("GET", "/bkt/k?attributes", "")
	h.mustStatus(resp, 400)
	body := h.readBody(resp)
	if !strings.Contains(body, "InvalidRequest") {
		t.Fatalf("expected InvalidRequest body: %s", body)
	}
}

func TestGetObjectAttributesUnknownAttributeReturns400(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt/k", "hello"), 200)

	resp := h.doString("GET", "/bkt/k?attributes", "",
		"x-amz-object-attributes", "ETag,WhatIsThis")
	h.mustStatus(resp, 400)
}

func TestGetObjectAttributesMultipartObjectParts(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	resp := h.doString("POST", "/bkt/mp?uploads", "")
	h.mustStatus(resp, 200)
	uploadID := uploadIDRE.FindStringSubmatch(h.readBody(resp))[1]

	var body strings.Builder
	body.WriteString("<CompleteMultipartUpload>")
	for i := 1; i <= 3; i++ {
		r := h.do("PUT", fmt.Sprintf("/bkt/mp?uploadId=%s&partNumber=%d", uploadID, i),
			byteReader([]byte(strings.Repeat("p", 32))))
		h.mustStatus(r, 200)
		etag := strings.Trim(r.Header.Get("Etag"), `"`)
		fmt.Fprintf(&body, `<Part><PartNumber>%d</PartNumber><ETag>"%s"</ETag></Part>`, i, etag)
	}
	body.WriteString("</CompleteMultipartUpload>")
	complete := h.doString("POST", "/bkt/mp?uploadId="+uploadID, body.String())
	h.mustStatus(complete, 200)
	_ = h.readBody(complete)

	attrs := h.doString("GET", "/bkt/mp?attributes", "",
		"x-amz-object-attributes", "ObjectParts")
	h.mustStatus(attrs, 200)
	got := h.readBody(attrs)
	if !strings.Contains(got, "<ObjectParts>") {
		t.Fatalf("expected ObjectParts subtree on multipart object; body=%s", got)
	}
	if !strings.Contains(got, "<PartsCount>3</PartsCount>") {
		t.Fatalf("expected PartsCount=3 on multipart object; body=%s", got)
	}
}
