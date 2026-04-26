package s3api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/crypto/master"
	datamem "github.com/danchupin/strata/internal/data/memory"
	metamem "github.com/danchupin/strata/internal/meta/memory"
	"github.com/danchupin/strata/internal/rewrap"
	"github.com/danchupin/strata/internal/s3api"
)

// rotationHarness hands back the harness plus the underlying meta store and
// rotation provider so a test can call rewrap.Run between PUTs.
type rotationHarness struct {
	*testHarness
	store    *metamem.Store
	dataB    *datamem.Backend
	provider *master.RotationProvider
}

func newRotationHarness(t *testing.T, entries []master.KeyEntry) *rotationHarness {
	t.Helper()
	mem := datamem.New()
	store := metamem.New()
	provider, err := master.NewRotationProvider(entries)
	if err != nil {
		t.Fatalf("rotation provider: %v", err)
	}
	api := s3api.New(mem, store)
	api.Region = "default"
	api.Master = provider
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p := r.Header.Get(testPrincipalHeader); p != "" {
			ctx := auth.WithAuth(r.Context(), &auth.AuthInfo{Owner: p, AccessKey: p})
			r = r.WithContext(ctx)
		}
		api.ServeHTTP(w, r)
	})
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return &rotationHarness{
		testHarness: &testHarness{t: t, ts: ts},
		store:       store,
		dataB:       mem,
		provider:    provider,
	}
}

func keyN(b byte) []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = b
	}
	return k
}

func TestSSERotation_PutUnderA_RotateToB_GetWorks(t *testing.T) {
	// Active key A; PUT an object; rotate so B becomes active and A is unwrap-only.
	h := newRotationHarness(t, []master.KeyEntry{
		{ID: "A", Key: keyN(0x11)},
	})
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	body := "rotation-body"
	h.mustStatus(h.doString("PUT", "/bkt/k", body, "x-amz-server-side-encryption", "AES256"), 200)

	// Simulate rotation: rebuild server with provider [B (active), A (legacy)].
	rotated, err := master.NewRotationProvider([]master.KeyEntry{
		{ID: "B", Key: keyN(0x22)},
		{ID: "A", Key: keyN(0x11)},
	})
	if err != nil {
		t.Fatalf("rotated provider: %v", err)
	}
	h.swapProvider(t, rotated)

	// Object was wrapped under A; ResolveByID must find it.
	resp := h.doString("GET", "/bkt/k", "")
	h.mustStatus(resp, 200)
	if got := h.readBody(resp); got != body {
		t.Fatalf("body mismatch: got %q", got)
	}

	// New PUTs after rotation must wrap under B.
	h.mustStatus(h.doString("PUT", "/bkt/k2", "post-rot", "x-amz-server-side-encryption", "AES256"), 200)
	bkt, _ := h.store.GetBucket(context.Background(), "bkt")
	o2, err := h.store.GetObject(context.Background(), bkt.ID, "k2", "")
	if err != nil {
		t.Fatalf("get k2: %v", err)
	}
	if o2.SSEKeyID != "B" {
		t.Fatalf("k2 wrapped under %q, want B (active)", o2.SSEKeyID)
	}
}

func TestSSERotation_GetFailsWithoutOldKey(t *testing.T) {
	// PUT under A, then drop A from the rotation list — GET must fail 500
	// rather than silently returning corrupt plaintext.
	h := newRotationHarness(t, []master.KeyEntry{{ID: "A", Key: keyN(0x11)}})
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt/k", "secret", "x-amz-server-side-encryption", "AES256"), 200)

	bOnly, err := master.NewRotationProvider([]master.KeyEntry{{ID: "B", Key: keyN(0x22)}})
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	h.swapProvider(t, bOnly)

	resp := h.doString("GET", "/bkt/k", "")
	if resp.StatusCode/100 == 2 {
		_ = resp.Body.Close()
		t.Fatalf("GET should fail when wrap key is missing; got %d", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

func TestSSERewrap_ConvertsAtoB(t *testing.T) {
	// PUT under A, rotate to B (with A still resolvable), run rewrap,
	// confirm objects now reference B and a re-run is a no-op.
	h := newRotationHarness(t, []master.KeyEntry{{ID: "A", Key: keyN(0x11)}})
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt/o1", "first", "x-amz-server-side-encryption", "AES256"), 200)
	h.mustStatus(h.doString("PUT", "/bkt/o2", "second", "x-amz-server-side-encryption", "AES256"), 200)

	rotated, err := master.NewRotationProvider([]master.KeyEntry{
		{ID: "B", Key: keyN(0x22)},
		{ID: "A", Key: keyN(0x11)},
	})
	if err != nil {
		t.Fatalf("rotated provider: %v", err)
	}
	h.swapProvider(t, rotated)

	w, err := rewrap.New(rewrap.Config{Meta: h.store, Provider: rotated})
	if err != nil {
		t.Fatalf("rewrap.New: %v", err)
	}
	stats, err := w.Run(context.Background())
	if err != nil {
		t.Fatalf("rewrap run: %v", err)
	}
	if stats.ObjectsRewrapped != 2 {
		t.Fatalf("ObjectsRewrapped = %d, want 2 (stats=%+v)", stats.ObjectsRewrapped, stats)
	}

	bkt, _ := h.store.GetBucket(context.Background(), "bkt")
	for _, k := range []string{"o1", "o2"} {
		o, err := h.store.GetObject(context.Background(), bkt.ID, k, "")
		if err != nil {
			t.Fatalf("get %s: %v", k, err)
		}
		if o.SSEKeyID != "B" {
			t.Fatalf("%s SSEKeyID = %q, want B", k, o.SSEKeyID)
		}
	}

	// Bytes still readable post-rewrap (DEK round-tripped correctly).
	resp := h.doString("GET", "/bkt/o1", "")
	h.mustStatus(resp, 200)
	if got := h.readBody(resp); got != "first" {
		t.Fatalf("o1 body after rewrap: got %q", got)
	}

	// Idempotency: second run touches nothing because the bucket is recorded
	// complete for target B AND every object already has SSEKeyID=B.
	stats2, err := w.Run(context.Background())
	if err != nil {
		t.Fatalf("rewrap second run: %v", err)
	}
	if stats2.BucketsSkipped != 1 || stats2.ObjectsRewrapped != 0 {
		t.Fatalf("second run not idempotent: %+v", stats2)
	}
}

func TestSSERewrap_ResumesPostRotation(t *testing.T) {
	// If rotation target changes between runs (B → C), the previously-recorded
	// "complete for B" must NOT cause the bucket to be skipped on the C pass.
	h := newRotationHarness(t, []master.KeyEntry{{ID: "A", Key: keyN(0x11)}})
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt/o", "x", "x-amz-server-side-encryption", "AES256"), 200)

	bToA, err := master.NewRotationProvider([]master.KeyEntry{
		{ID: "B", Key: keyN(0x22)},
		{ID: "A", Key: keyN(0x11)},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.swapProvider(t, bToA)
	w, _ := rewrap.New(rewrap.Config{Meta: h.store, Provider: bToA})
	if _, err := w.Run(context.Background()); err != nil {
		t.Fatalf("first rewrap: %v", err)
	}

	// Now rotate to C with B + A still available.
	cToBA, err := master.NewRotationProvider([]master.KeyEntry{
		{ID: "C", Key: keyN(0x33)},
		{ID: "B", Key: keyN(0x22)},
		{ID: "A", Key: keyN(0x11)},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.swapProvider(t, cToBA)
	w2, _ := rewrap.New(rewrap.Config{Meta: h.store, Provider: cToBA})
	stats, err := w2.Run(context.Background())
	if err != nil {
		t.Fatalf("second rewrap: %v", err)
	}
	if stats.ObjectsRewrapped != 1 {
		t.Fatalf("expected 1 rewrap (B → C), got %+v", stats)
	}
	bkt, _ := h.store.GetBucket(context.Background(), "bkt")
	o, _ := h.store.GetObject(context.Background(), bkt.ID, "o", "")
	if o.SSEKeyID != "C" {
		t.Fatalf("SSEKeyID = %q, want C", o.SSEKeyID)
	}
}

// swapProvider rebuilds the underlying *s3api.Server with a new master
// provider, keeping the existing meta + data backends. Used by rotation tests
// to simulate gateway restart with a different STRATA_SSE_MASTER_KEYS value.
func (h *rotationHarness) swapProvider(t *testing.T, p *master.RotationProvider) {
	t.Helper()
	api := s3api.New(h.dataBackend(), h.store)
	api.Region = "default"
	api.Master = p
	h.provider = p
	h.ts.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if pr := r.Header.Get(testPrincipalHeader); pr != "" {
			ctx := auth.WithAuth(r.Context(), &auth.AuthInfo{Owner: pr, AccessKey: pr})
			r = r.WithContext(ctx)
		}
		api.ServeHTTP(w, r)
	})
}

func (h *rotationHarness) dataBackend() *datamem.Backend {
	return h.dataB
}
