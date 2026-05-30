package s3api_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/crypto/kms"
	datamem "github.com/danchupin/strata/internal/data/memory"
	metamem "github.com/danchupin/strata/internal/meta/memory"
	"github.com/danchupin/strata/internal/s3api"
)

// kmsHarness wires an in-memory gateway with a deterministic LocalHSMProvider
// standing in for AWS KMS. It exposes the meta store so a test can mutate the
// persisted SSEKeyID to drive the wrong-key-id (IncorrectKeyException) path
// without talking to a real KMS.
type kmsHarness struct {
	*testHarness
	store *metamem.Store
}

func newKMSHarness(t *testing.T) *kmsHarness {
	t.Helper()
	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = byte(0x5A + i)
	}
	prov, err := kms.NewLocalHSMProvider(seed)
	if err != nil {
		t.Fatalf("local-hsm provider: %v", err)
	}
	mem := datamem.New()
	store := metamem.New()
	api := s3api.New(mem, store)
	api.Region = "default"
	api.KMS = prov
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p := r.Header.Get(testPrincipalHeader); p != "" {
			ctx := auth.WithAuth(r.Context(), &auth.AuthInfo{Owner: p, AccessKey: p})
			r = r.WithContext(ctx)
		}
		api.ServeHTTP(w, r)
	})
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return &kmsHarness{testHarness: &testHarness{t: t, ts: ts}, store: store}
}

// TestSSEKMS_RoundTrip is a thin positive control: PUT under aws:kms with a key
// id round-trips and echoes the key id on GET. Kept minimal — the SSE-S3 happy
// path is already covered in sse_object_test.go; this only proves the KMS DEK
// wrap/unwrap wiring works so the negative tests below are meaningful.
func TestSSEKMS_RoundTrip(t *testing.T) {
	h := newKMSHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	body := "kms payload"
	resp := h.doString("PUT", "/bkt/k", body,
		"x-amz-server-side-encryption", "aws:kms",
		"x-amz-server-side-encryption-aws-kms-key-id", "kms-key-1")
	h.mustStatus(resp, 200)
	if got := resp.Header.Get("x-amz-server-side-encryption"); got != "aws:kms" {
		t.Fatalf("PUT sse echo: got %q", got)
	}
	if got := resp.Header.Get("x-amz-server-side-encryption-aws-kms-key-id"); got != "kms-key-1" {
		t.Fatalf("PUT key-id echo: got %q", got)
	}

	resp = h.doString("GET", "/bkt/k", "")
	h.mustStatus(resp, 200)
	if got := resp.Header.Get("x-amz-server-side-encryption-aws-kms-key-id"); got != "kms-key-1" {
		t.Fatalf("GET key-id echo: got %q", got)
	}
	if got := h.readBody(resp); got != body {
		t.Fatalf("GET body: got %q want %q", got, body)
	}
}

// TestSSEKMS_WrongKeyID_AccessDenied drives the IncorrectKeyException path:
// the object is wrapped under "kms-key-1", but the persisted key id is then
// flipped to "kms-key-2". On GET the gateway unwraps with the stored (wrong)
// id, the LocalHSM mac recompute fails → kms.ErrKeyIDMismatch → the gateway
// must map it to 403 AccessDenied (ErrKMSAccessDenied), NOT silently return
// plaintext or a 500.
func TestSSEKMS_WrongKeyID_AccessDenied(t *testing.T) {
	h := newKMSHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt/k", "secret",
		"x-amz-server-side-encryption", "aws:kms",
		"x-amz-server-side-encryption-aws-kms-key-id", "kms-key-1"), 200)

	bkt, err := h.store.GetBucket(context.Background(), "bkt")
	if err != nil {
		t.Fatalf("get bucket: %v", err)
	}
	o, err := h.store.GetObject(context.Background(), bkt.ID, "k", "")
	if err != nil {
		t.Fatalf("get object: %v", err)
	}
	// Keep the wrapped bytes (wrapped under kms-key-1) but point the row at a
	// different key id — the mac will not validate under kms-key-2.
	if err := h.store.UpdateObjectSSEWrap(context.Background(), bkt.ID, "k", "", o.SSEKey, "kms-key-2"); err != nil {
		t.Fatalf("update sse wrap: %v", err)
	}

	resp := h.doString("GET", "/bkt/k", "")
	h.mustStatus(resp, 403)
	if body := h.readBody(resp); !strings.Contains(body, "AccessDenied") {
		t.Fatalf("expected AccessDenied, got: %s", body)
	}
}

// TestSSEKMS_MultipartPerPartDecrypt completes a 3-part multipart upload under
// aws:kms and asserts the whole object decrypts. The per-part chunk locator
// (Manifest.PartChunkCounts) is what makes this work — each part was encrypted
// with oid = "<key>:part=<n>" and chunk-index restarting at 0, so a wrong
// locator would AEAD-fail the second part onward. We additionally assert the
// persisted manifest carries one PartChunkCounts entry per part.
func TestSSEKMS_MultipartPerPartDecrypt(t *testing.T) {
	h := newKMSHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	resp := h.doString("POST", "/bkt/mp?uploads=", "",
		"x-amz-server-side-encryption", "aws:kms",
		"x-amz-server-side-encryption-aws-kms-key-id", "kms-key-1")
	h.mustStatus(resp, 200)
	if got := resp.Header.Get("x-amz-server-side-encryption"); got != "aws:kms" {
		t.Fatalf("Initiate sse echo: got %q", got)
	}
	m := ssempUploadIDRE.FindStringSubmatch(h.readBody(resp))
	if len(m) != 2 {
		t.Fatalf("no UploadId")
	}
	uploadID := m[1]

	r := rand.New(rand.NewSource(0xA11CE))
	parts := [][]byte{
		makeBytes(r, 5*1024*1024),
		makeBytes(r, 5*1024*1024),
		makeBytes(r, 1024*1024),
	}
	var orig []byte
	var cb strings.Builder
	cb.WriteString("<CompleteMultipartUpload>")
	for i, p := range parts {
		pnum := i + 1
		url := fmt.Sprintf("/bkt/mp?uploadId=%s&partNumber=%d", uploadID, pnum)
		pr := h.do("PUT", url, bytes.NewReader(p))
		h.mustStatus(pr, 200)
		etag := strings.Trim(pr.Header.Get("ETag"), `"`)
		cb.WriteString(fmt.Sprintf(`<Part><PartNumber>%d</PartNumber><ETag>"%s"</ETag></Part>`, pnum, etag))
		orig = append(orig, p...)
	}
	cb.WriteString("</CompleteMultipartUpload>")

	resp = h.doString("POST", "/bkt/mp?uploadId="+uploadID, cb.String())
	h.mustStatus(resp, 200)

	// Whole-object GET must decrypt every part via the per-part locator.
	resp = h.do("GET", "/bkt/mp", nil)
	h.mustStatus(resp, http.StatusOK)
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	_ = resp.Body.Close()
	if !bytes.Equal(got, orig) {
		t.Fatalf("body mismatch: got %d bytes, want %d, first-diff %d", len(got), len(orig), firstDiff(got, orig))
	}

	// Assert the per-part locator metadata is actually persisted: one
	// PartChunkCounts entry per uploaded part, all positive.
	bkt, err := h.store.GetBucket(context.Background(), "bkt")
	if err != nil {
		t.Fatalf("get bucket: %v", err)
	}
	o, err := h.store.GetObject(context.Background(), bkt.ID, "mp", "")
	if err != nil {
		t.Fatalf("get object: %v", err)
	}
	if o.Manifest == nil {
		t.Fatalf("manifest nil on completed multipart object")
	}
	if len(o.Manifest.PartChunkCounts) != len(parts) {
		t.Fatalf("PartChunkCounts len = %d, want %d", len(o.Manifest.PartChunkCounts), len(parts))
	}
	for i, c := range o.Manifest.PartChunkCounts {
		if c <= 0 {
			t.Fatalf("PartChunkCounts[%d] = %d, want > 0", i, c)
		}
	}
}
