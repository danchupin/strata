package s3api_test

import (
	"crypto/md5"
	"encoding/base64"
	"strings"
	"testing"
)

func ssecHeaders(key []byte) (algo, keyB64, keyMD5 string) {
	algo = "AES256"
	keyB64 = base64.StdEncoding.EncodeToString(key)
	sum := md5.Sum(key)
	keyMD5 = base64.StdEncoding.EncodeToString(sum[:])
	return
}

func makeKey(b byte) []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = b
	}
	return k
}

func TestSSECPutGetRoundTrip(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	key := makeKey(0x42)
	algo, keyB64, keyMD5 := ssecHeaders(key)

	resp := h.doString("PUT", "/bkt/k", "payload",
		"x-amz-server-side-encryption-customer-algorithm", algo,
		"x-amz-server-side-encryption-customer-key", keyB64,
		"x-amz-server-side-encryption-customer-key-MD5", keyMD5)
	h.mustStatus(resp, 200)

	resp = h.doString("GET", "/bkt/k", "",
		"x-amz-server-side-encryption-customer-algorithm", algo,
		"x-amz-server-side-encryption-customer-key", keyB64,
		"x-amz-server-side-encryption-customer-key-MD5", keyMD5)
	h.mustStatus(resp, 200)
	if got := resp.Header.Get("x-amz-server-side-encryption-customer-algorithm"); got != "AES256" {
		t.Fatalf("GET algo header: got %q want AES256", got)
	}
	if got := resp.Header.Get("x-amz-server-side-encryption-customer-key-MD5"); got != keyMD5 {
		t.Fatalf("GET keyMD5 header: got %q want %q", got, keyMD5)
	}
	if body := h.readBody(resp); body != "payload" {
		t.Fatalf("GET body: got %q want %q", body, "payload")
	}

	resp = h.doString("HEAD", "/bkt/k", "",
		"x-amz-server-side-encryption-customer-algorithm", algo,
		"x-amz-server-side-encryption-customer-key", keyB64,
		"x-amz-server-side-encryption-customer-key-MD5", keyMD5)
	h.mustStatus(resp, 200)
}

func TestSSECGetMissingKey(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	key := makeKey(0x42)
	algo, keyB64, keyMD5 := ssecHeaders(key)
	h.mustStatus(h.doString("PUT", "/bkt/k", "payload",
		"x-amz-server-side-encryption-customer-algorithm", algo,
		"x-amz-server-side-encryption-customer-key", keyB64,
		"x-amz-server-side-encryption-customer-key-MD5", keyMD5), 200)

	resp := h.doString("GET", "/bkt/k", "")
	h.mustStatus(resp, 400)
	if body := h.readBody(resp); !strings.Contains(body, "InvalidRequest") {
		t.Fatalf("expected InvalidRequest, got: %s", body)
	}
}

func TestSSECPutWrongMD5(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	key := makeKey(0x42)
	_, keyB64, _ := ssecHeaders(key)
	otherKey := makeKey(0x99)
	_, _, otherMD5 := ssecHeaders(otherKey)

	resp := h.doString("PUT", "/bkt/k", "payload",
		"x-amz-server-side-encryption-customer-algorithm", "AES256",
		"x-amz-server-side-encryption-customer-key", keyB64,
		"x-amz-server-side-encryption-customer-key-MD5", otherMD5)
	h.mustStatus(resp, 400)
	if body := h.readBody(resp); !strings.Contains(body, "InvalidDigest") {
		t.Fatalf("expected InvalidDigest, got: %s", body)
	}

	resp = h.doString("GET", "/bkt/k", "")
	h.mustStatus(resp, 404)
}

func TestSSECGetWrongMD5(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	key := makeKey(0x42)
	algo, keyB64, keyMD5 := ssecHeaders(key)
	h.mustStatus(h.doString("PUT", "/bkt/k", "payload",
		"x-amz-server-side-encryption-customer-algorithm", algo,
		"x-amz-server-side-encryption-customer-key", keyB64,
		"x-amz-server-side-encryption-customer-key-MD5", keyMD5), 200)

	other := makeKey(0x99)
	_, otherB64, otherMD5 := ssecHeaders(other)
	resp := h.doString("GET", "/bkt/k", "",
		"x-amz-server-side-encryption-customer-algorithm", "AES256",
		"x-amz-server-side-encryption-customer-key", otherB64,
		"x-amz-server-side-encryption-customer-key-MD5", otherMD5)
	h.mustStatus(resp, 400)
	if body := h.readBody(resp); !strings.Contains(body, "AccessDenied") {
		t.Fatalf("expected AccessDenied, got: %s", body)
	}
}

func TestSSECPutInvalidKeyLength(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	short := make([]byte, 16)
	shortB64 := base64.StdEncoding.EncodeToString(short)
	sum := md5.Sum(short)
	shortMD5 := base64.StdEncoding.EncodeToString(sum[:])

	resp := h.doString("PUT", "/bkt/k", "payload",
		"x-amz-server-side-encryption-customer-algorithm", "AES256",
		"x-amz-server-side-encryption-customer-key", shortB64,
		"x-amz-server-side-encryption-customer-key-MD5", shortMD5)
	h.mustStatus(resp, 400)
	if body := h.readBody(resp); !strings.Contains(body, "InvalidArgument") {
		t.Fatalf("expected InvalidArgument, got: %s", body)
	}
}

func TestSSECPutBadAlgorithm(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	key := makeKey(0x42)
	_, keyB64, keyMD5 := ssecHeaders(key)

	resp := h.doString("PUT", "/bkt/k", "payload",
		"x-amz-server-side-encryption-customer-algorithm", "DES",
		"x-amz-server-side-encryption-customer-key", keyB64,
		"x-amz-server-side-encryption-customer-key-MD5", keyMD5)
	h.mustStatus(resp, 400)
}

func TestSSECPartialHeadersRejected(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	resp := h.doString("PUT", "/bkt/k", "payload",
		"x-amz-server-side-encryption-customer-algorithm", "AES256")
	h.mustStatus(resp, 400)
	if body := h.readBody(resp); !strings.Contains(body, "InvalidRequest") {
		t.Fatalf("expected InvalidRequest, got: %s", body)
	}
}

func TestSSECCopyObjectMirrorHeaders(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	srcKey := makeKey(0x42)
	algo, srcB64, srcMD5 := ssecHeaders(srcKey)
	h.mustStatus(h.doString("PUT", "/bkt/src", "payload",
		"x-amz-server-side-encryption-customer-algorithm", algo,
		"x-amz-server-side-encryption-customer-key", srcB64,
		"x-amz-server-side-encryption-customer-key-MD5", srcMD5), 200)

	resp := h.doString("PUT", "/bkt/dst", "",
		"x-amz-copy-source", "/bkt/src",
		"x-amz-copy-source-server-side-encryption-customer-algorithm", algo,
		"x-amz-copy-source-server-side-encryption-customer-key", srcB64,
		"x-amz-copy-source-server-side-encryption-customer-key-MD5", srcMD5)
	h.mustStatus(resp, 200)

	resp = h.doString("HEAD", "/bkt/dst", "")
	h.mustStatus(resp, 200)

	resp = h.doString("PUT", "/bkt/dst2", "",
		"x-amz-copy-source", "/bkt/src")
	h.mustStatus(resp, 400)
	if body := h.readBody(resp); !strings.Contains(body, "InvalidRequest") {
		t.Fatalf("expected InvalidRequest, got: %s", body)
	}
}
