package s3api_test

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"net/http"
	"strings"
	"testing"

	"github.com/danchupin/strata/internal/s3api"
)

// TestMultipartCopyCRC64NVMEDefaultsToFullObject reproduces the s3-tests
// `test_multipart_copy_*` failure: boto3 1.36+ FlexibleChecksum default-on
// emits `x-amz-checksum-algorithm=CRC64NVME` on Initiate without an explicit
// `x-amz-checksum-type`. CRC64NVME has no COMPOSITE shape on AWS, so the
// gateway must persist FULL_OBJECT — otherwise GET-side body recompute fires
// `FlexibleChecksumError`. This is RED before the Initiate-time defaulting
// fix and GREEN after.
func TestMultipartCopyCRC64NVMEDefaultsToFullObject(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	body := strings.Repeat("c", 6<<20)
	h.mustStatus(h.do("PUT", "/bkt/src", strings.NewReader(body)), 200)

	resp := h.doString("POST", "/bkt/dst?uploads", "",
		"x-amz-checksum-algorithm", "CRC64NVME")
	h.mustStatus(resp, http.StatusOK)
	initiateBody := h.readBody(resp)
	if !strings.Contains(initiateBody, "<ChecksumType>FULL_OBJECT</ChecksumType>") {
		t.Fatalf("Initiate body missing ChecksumType=FULL_OBJECT: %s", initiateBody)
	}
	uploadID := uploadIDRE.FindStringSubmatch(initiateBody)[1]

	resp = h.doString("PUT", "/bkt/dst?uploadId="+uploadID+"&partNumber=1", "",
		"x-amz-copy-source", "/bkt/src",
		"x-amz-checksum-algorithm", "CRC64NVME")
	h.mustStatus(resp, http.StatusOK)
	etag := strings.Trim(resp.Header.Get("Etag"), `"`)

	complete := "<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>\"" + etag + "\"</ETag></Part></CompleteMultipartUpload>"
	resp = h.doString("POST", "/bkt/dst?uploadId="+uploadID, complete)
	h.mustStatus(resp, http.StatusOK)
	cb := h.readBody(resp)
	if !strings.Contains(cb, "<ChecksumType>FULL_OBJECT</ChecksumType>") {
		t.Fatalf("Complete body missing ChecksumType=FULL_OBJECT: %s", cb)
	}

	get := h.doString("GET", "/bkt/dst", "", "x-amz-checksum-mode", "ENABLED")
	h.mustStatus(get, http.StatusOK)
	gotBody := h.readBody(get)
	if gotBody != body {
		t.Fatalf("GET body length=%d want=%d", len(gotBody), len(body))
	}
	if got := get.Header.Get("x-amz-checksum-type"); got != "FULL_OBJECT" {
		t.Errorf("GET x-amz-checksum-type: got %q want FULL_OBJECT", got)
	}

	var b [8]byte
	binary.BigEndian.PutUint64(b[:], s3api.CRC64NVMEForTest([]byte(body)))
	want := base64.StdEncoding.EncodeToString(b[:])
	got := get.Header.Get("x-amz-checksum-crc64nvme")
	if got != want {
		t.Fatalf("GET x-amz-checksum-crc64nvme: got %q want %q (raw CRC over body)", got, want)
	}
	if strings.HasSuffix(got, "-1") {
		t.Fatalf("FULL_OBJECT shape must not have -N suffix: got %q", got)
	}
}

// TestMultipartInitiateRejectsCRC64NVMEComposite asserts AWS-parity: CRC64NVME
// has no COMPOSITE shape on AWS, so an explicit pairing is rejected at
// Initiate.
func TestMultipartInitiateRejectsCRC64NVMEComposite(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	resp := h.doString("POST", "/bkt/k?uploads", "",
		"x-amz-checksum-algorithm", "CRC64NVME",
		"x-amz-checksum-type", "COMPOSITE")
	h.mustStatus(resp, http.StatusBadRequest)
	if !strings.Contains(h.readBody(resp), "<Code>InvalidArgument</Code>") {
		t.Fatalf("expected InvalidArgument on CRC64NVME+COMPOSITE pairing")
	}
}

// TestMultipartCRC32DefaultsToFullObject locks in the CRC32 / CRC32C empty-type
// defaulting that mirrors modern boto3 (FlexibleChecksum default-on): no
// explicit type → FULL_OBJECT, GET emits raw whole-stream CRC with no -N
// suffix and matches a recompute over the body bytes.
func TestMultipartCRC32DefaultsToFullObject(t *testing.T) {
	for _, algo := range []string{"CRC32", "CRC32C"} {
		t.Run(algo, func(t *testing.T) {
			hdr := "x-amz-checksum-" + strings.ToLower(algo)
			h := newHarness(t)
			h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
			resp := h.doString("POST", "/bkt/k?uploads", "",
				"x-amz-checksum-algorithm", algo)
			h.mustStatus(resp, http.StatusOK)
			initiateBody := h.readBody(resp)
			if !strings.Contains(initiateBody, "<ChecksumType>FULL_OBJECT</ChecksumType>") {
				t.Fatalf("%s Initiate body missing ChecksumType=FULL_OBJECT: %s", algo, initiateBody)
			}
			uploadID := uploadIDRE.FindStringSubmatch(initiateBody)[1]

			parts := [][]byte{
				[]byte(strings.Repeat("A", 1024)),
				[]byte(strings.Repeat("B", 1024)),
			}
			partB64 := make([]string, len(parts))
			var completeBody strings.Builder
			completeBody.WriteString("<CompleteMultipartUpload>")
			for i, p := range parts {
				pn := i + 1
				partB64[i] = partChecksum(t, algo, p)
				url := fmt.Sprintf("/bkt/k?uploadId=%s&partNumber=%d", uploadID, pn)
				r := h.do("PUT", url, byteReader(p), hdr, partB64[i])
				h.mustStatus(r, http.StatusOK)
				etag := strings.Trim(r.Header.Get("Etag"), `"`)
				fmt.Fprintf(&completeBody, `<Part><PartNumber>%d</PartNumber><ETag>"%s"</ETag></Part>`, pn, etag)
			}
			completeBody.WriteString("</CompleteMultipartUpload>")
			complete := h.doString("POST", "/bkt/k?uploadId="+uploadID, completeBody.String())
			h.mustStatus(complete, http.StatusOK)
			completeStr := h.readBody(complete)
			if !strings.Contains(completeStr, "<ChecksumType>FULL_OBJECT</ChecksumType>") {
				t.Fatalf("%s Complete body missing ChecksumType=FULL_OBJECT: %s", algo, completeStr)
			}

			var concat []byte
			for _, p := range parts {
				concat = append(concat, p...)
			}
			var b [4]byte
			switch algo {
			case "CRC32":
				binary.BigEndian.PutUint32(b[:], crc32.ChecksumIEEE(concat))
			case "CRC32C":
				binary.BigEndian.PutUint32(b[:], crc32.Checksum(concat, crc32.MakeTable(crc32.Castagnoli)))
			}
			want := base64.StdEncoding.EncodeToString(b[:])

			get := h.doString("GET", "/bkt/k", "", "x-amz-checksum-mode", "ENABLED")
			h.mustStatus(get, http.StatusOK)
			_ = h.readBody(get)
			if got := get.Header.Get(hdr); got != want {
				t.Fatalf("%s GET %s: got %q want %q (FULL_OBJECT shape)", algo, hdr, got, want)
			}
			if strings.Contains(get.Header.Get(hdr), "-2") {
				t.Fatalf("%s FULL_OBJECT shape must not have -N suffix; got %q", algo, get.Header.Get(hdr))
			}
		})
	}
}
