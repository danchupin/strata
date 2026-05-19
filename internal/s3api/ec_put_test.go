package s3api_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/danchupin/strata/internal/meta"
)

// TestPutStampsManifestECParamsFromBucket verifies the PUT hot path picks
// up bucket.ECPolicy and stamps Manifest.ECParams accordingly (US-007
// EC-aware manifests). The bucket policy is seeded via the meta store
// directly so the test bypasses backend capability probing — the admin
// handler enforces that gate, the PUT path only reads what's persisted.
func TestPutStampsManifestECParamsFromBucket(t *testing.T) {
	h := newHarness(t)
	resp := h.doString(http.MethodPut, "/ecbkt", "", testPrincipalHeader, "alice")
	h.mustStatus(resp, http.StatusOK)
	_ = h.readBody(resp)

	if err := h.meta.SetBucketECPolicy(context.Background(), "ecbkt", meta.ECPolicy{K: 4, M: 2}); err != nil {
		t.Fatalf("seed ec policy: %v", err)
	}

	resp = h.doString(http.MethodPut, "/ecbkt/obj", "hello-ec",
		testPrincipalHeader, "alice")
	h.mustStatus(resp, http.StatusOK)
	_ = h.readBody(resp)

	obj, err := h.meta.GetObject(context.Background(), bucketID(t, h, "ecbkt"), "obj", "")
	if err != nil {
		t.Fatalf("get object: %v", err)
	}
	if obj.Manifest == nil || obj.Manifest.ECParams == nil {
		t.Fatalf("manifest.ECParams nil; want k=4 m=2; manifest=%+v", obj.Manifest)
	}
	if obj.Manifest.ECParams.K != 4 || obj.Manifest.ECParams.M != 2 {
		t.Errorf("manifest.ECParams: got %+v want {K:4 M:2}", obj.Manifest.ECParams)
	}
}

// TestPutWithoutECPolicyLeavesECParamsNil pins the negative path so a
// bucket with no policy doesn't accidentally stamp zero-value ECParams.
func TestPutWithoutECPolicyLeavesECParamsNil(t *testing.T) {
	h := newHarness(t)
	resp := h.doString(http.MethodPut, "/plain", "", testPrincipalHeader, "alice")
	h.mustStatus(resp, http.StatusOK)
	_ = h.readBody(resp)

	resp = h.doString(http.MethodPut, "/plain/obj", "hi",
		testPrincipalHeader, "alice")
	h.mustStatus(resp, http.StatusOK)
	_ = h.readBody(resp)

	obj, err := h.meta.GetObject(context.Background(), bucketID(t, h, "plain"), "obj", "")
	if err != nil {
		t.Fatalf("get object: %v", err)
	}
	if obj.Manifest != nil && obj.Manifest.ECParams != nil {
		t.Errorf("manifest.ECParams: got %+v want nil", obj.Manifest.ECParams)
	}
}

func bucketID(t *testing.T, h *testHarness, name string) (id [16]byte) {
	t.Helper()
	b, err := h.meta.GetBucket(context.Background(), name)
	if err != nil {
		t.Fatalf("get bucket %q: %v", name, err)
	}
	return b.ID
}
