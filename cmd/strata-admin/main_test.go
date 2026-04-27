package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/crypto/master"
	datamem "github.com/danchupin/strata/internal/data/memory"
	"github.com/danchupin/strata/internal/meta"
	metamem "github.com/danchupin/strata/internal/meta/memory"
	"github.com/danchupin/strata/internal/s3api"
)

const testPrincipalHeader = "X-Test-Principal"

type gatewayHarness struct {
	ts       *httptest.Server
	store    *metamem.Store
	api      *s3api.Server
}

func newGatewayHarness(t *testing.T, masterProv master.Provider) *gatewayHarness {
	t.Helper()
	store := metamem.New()
	api := s3api.New(datamem.New(), store)
	api.Region = "default"
	if masterProv != nil {
		api.Master = masterProv
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p := r.Header.Get(testPrincipalHeader); p != "" {
			ctx := auth.WithAuth(r.Context(), &auth.AuthInfo{Owner: p, AccessKey: p})
			r = r.WithContext(ctx)
		}
		api.ServeHTTP(w, r)
	})
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return &gatewayHarness{ts: ts, store: store, api: api}
}

func (h *gatewayHarness) rootClient() *Client {
	return &Client{Endpoint: h.ts.URL, Principal: s3api.IAMRootPrincipal}
}

func TestClient_CreateAccessKey(t *testing.T) {
	h := newGatewayHarness(t, nil)
	if err := h.store.CreateIAMUser(context.Background(), &meta.IAMUser{UserName: "alice"}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	c := h.rootClient()
	ak, err := c.CreateAccessKey(context.Background(), "alice")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if ak.UserName != "alice" {
		t.Fatalf("UserName=%q", ak.UserName)
	}
	if !strings.HasPrefix(ak.AccessKeyID, "AKIA") {
		t.Fatalf("access key id %q", ak.AccessKeyID)
	}
	if ak.SecretAccessKey == "" {
		t.Fatalf("secret missing")
	}
	if ak.Status != "Active" {
		t.Fatalf("status=%q", ak.Status)
	}
}

func TestClient_RotateAccessKey(t *testing.T) {
	h := newGatewayHarness(t, nil)
	ctx := context.Background()
	if err := h.store.CreateIAMUser(ctx, &meta.IAMUser{UserName: "alice"}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	c := h.rootClient()
	old, err := c.CreateAccessKey(ctx, "alice")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	rotated, err := c.RotateAccessKey(ctx, old.AccessKeyID)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if rotated.AccessKeyID == old.AccessKeyID {
		t.Fatalf("rotation kept same id %q", rotated.AccessKeyID)
	}
	if rotated.UserName != "alice" {
		t.Fatalf("rotated user=%q", rotated.UserName)
	}
	// Old key gone.
	if _, err := h.store.GetIAMAccessKey(ctx, old.AccessKeyID); err == nil {
		t.Fatalf("old access key still present")
	}
	// New key present.
	if _, err := h.store.GetIAMAccessKey(ctx, rotated.AccessKeyID); err != nil {
		t.Fatalf("new key not stored: %v", err)
	}
}

func TestClient_LifecycleTick(t *testing.T) {
	h := newGatewayHarness(t, nil)
	c := h.rootClient()
	res, err := c.LifecycleTick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if !res.OK {
		t.Fatalf("not ok: %+v", res)
	}
}

func TestClient_GCDrain(t *testing.T) {
	h := newGatewayHarness(t, nil)
	c := h.rootClient()
	res, err := c.GCDrain(context.Background())
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if !res.OK {
		t.Fatalf("not ok: %+v", res)
	}
	if res.Drained != 0 {
		t.Fatalf("expected 0 drained on empty queue, got %d", res.Drained)
	}
}

func TestClient_SSERotate_NoRotationProvider(t *testing.T) {
	h := newGatewayHarness(t, nil)
	c := h.rootClient()
	if _, err := c.SSERotate(context.Background()); err == nil {
		t.Fatalf("expected error when no rotation provider")
	} else if hErr, ok := err.(*HTTPError); !ok || hErr.Status != http.StatusBadRequest {
		t.Fatalf("expected 400, got %v", err)
	}
}

func TestClient_SSERotate(t *testing.T) {
	rot, err := master.NewRotationProvider([]master.KeyEntry{
		{ID: "k1", Key: bytes.Repeat([]byte{0x01}, 32)},
	})
	if err != nil {
		t.Fatalf("rotation provider: %v", err)
	}
	h := newGatewayHarness(t, rot)
	c := h.rootClient()
	res, err := c.SSERotate(context.Background())
	if err != nil {
		t.Fatalf("sse rotate: %v", err)
	}
	if !res.OK {
		t.Fatalf("not ok: %+v", res)
	}
	if res.ActiveID != "k1" {
		t.Fatalf("active id=%q", res.ActiveID)
	}
}

func TestClient_ReplicateRetry_UnknownBucket(t *testing.T) {
	h := newGatewayHarness(t, nil)
	c := h.rootClient()
	if _, err := c.ReplicateRetry(context.Background(), "nope"); err == nil {
		t.Fatalf("expected NoSuchBucket")
	} else if hErr, ok := err.(*HTTPError); !ok || hErr.Status != http.StatusNotFound {
		t.Fatalf("expected 404, got %v", err)
	}
}

func TestClient_ReplicateRetry_NoFailedRows(t *testing.T) {
	h := newGatewayHarness(t, nil)
	if _, err := h.store.CreateBucket(context.Background(), "bkt", s3api.IAMRootPrincipal, "STANDARD"); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	c := h.rootClient()
	res, err := c.ReplicateRetry(context.Background(), "bkt")
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if res.Bucket != "bkt" {
		t.Fatalf("bucket=%q", res.Bucket)
	}
	if res.Requeued != 0 {
		t.Fatalf("requeued=%d", res.Requeued)
	}
}

func TestClient_BucketInspect(t *testing.T) {
	h := newGatewayHarness(t, nil)
	ctx := context.Background()
	if _, err := h.store.CreateBucket(ctx, "bkt", "alice", "STANDARD"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := h.store.SetBucketVersioning(ctx, "bkt", meta.VersioningEnabled); err != nil {
		t.Fatalf("versioning: %v", err)
	}
	b, _ := h.store.GetBucket(ctx, "bkt")
	corsBlob := []byte(`<CORSConfiguration><CORSRule><AllowedOrigin>*</AllowedOrigin><AllowedMethod>GET</AllowedMethod></CORSRule></CORSConfiguration>`)
	if err := h.store.SetBucketCORS(ctx, b.ID, corsBlob); err != nil {
		t.Fatalf("cors: %v", err)
	}

	c := h.rootClient()
	res, err := c.BucketInspect(ctx, "bkt")
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if res.Name != "bkt" {
		t.Fatalf("name=%q", res.Name)
	}
	if res.Owner != "alice" {
		t.Fatalf("owner=%q", res.Owner)
	}
	if res.Versioning != meta.VersioningEnabled {
		t.Fatalf("versioning=%q", res.Versioning)
	}
	if _, ok := res.Configs["cors"]; !ok {
		t.Fatalf("cors config missing from inspect: %+v", res.Configs)
	}
}

func TestClient_AdminEndpointsRequireRoot(t *testing.T) {
	h := newGatewayHarness(t, nil)
	c := &Client{Endpoint: h.ts.URL, Principal: "alice"}
	if _, err := c.LifecycleTick(context.Background()); err == nil {
		t.Fatalf("non-root must be denied")
	} else if hErr, ok := err.(*HTTPError); !ok || hErr.Status != http.StatusForbidden {
		t.Fatalf("expected 403, got %v", err)
	}
}

func TestClient_AdminEndpointsRequireAuth(t *testing.T) {
	h := newGatewayHarness(t, nil)
	c := &Client{Endpoint: h.ts.URL}
	if _, err := c.LifecycleTick(context.Background()); err == nil {
		t.Fatalf("anonymous must be denied")
	} else if hErr, ok := err.(*HTTPError); !ok || hErr.Status != http.StatusForbidden {
		t.Fatalf("expected 403, got %v", err)
	}
}

func TestClient_AdminEndpointsRejectPresignedURL(t *testing.T) {
	h := newGatewayHarness(t, nil)
	req, _ := http.NewRequest(http.MethodPost,
		h.ts.URL+"/admin/lifecycle/tick?X-Amz-Signature=abc",
		nil,
	)
	req.Header.Set(testPrincipalHeader, s3api.IAMRootPrincipal)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("req: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for presigned admin call, got %d", resp.StatusCode)
	}
}

// TestApp_RunIAMCreate exercises the argv dispatch path end-to-end.
func TestApp_RunIAMCreate(t *testing.T) {
	h := newGatewayHarness(t, nil)
	if err := h.store.CreateIAMUser(context.Background(), &meta.IAMUser{UserName: "alice"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var stdout, stderr bytes.Buffer
	a := newApp(&stdout, &stderr, []string{
		"--endpoint", h.ts.URL,
		"--principal", s3api.IAMRootPrincipal,
		"--json",
		"iam", "create-access-key", "--user", "alice",
	})
	if err := a.run(context.Background()); err != nil {
		t.Fatalf("run: %v stderr=%s", err, stderr.String())
	}
	var ak AccessKey
	if err := json.Unmarshal(stdout.Bytes(), &ak); err != nil {
		t.Fatalf("decode stdout %q: %v", stdout.String(), err)
	}
	if ak.UserName != "alice" || ak.SecretAccessKey == "" {
		t.Fatalf("ak=%+v", ak)
	}
}

func TestApp_RunBucketInspect_HumanOutput(t *testing.T) {
	h := newGatewayHarness(t, nil)
	if _, err := h.store.CreateBucket(context.Background(), "bkt", "alice", "STANDARD"); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	var stdout, stderr bytes.Buffer
	a := newApp(&stdout, &stderr, []string{
		"--endpoint", h.ts.URL,
		"--principal", s3api.IAMRootPrincipal,
		"bucket", "inspect", "--bucket", "bkt",
	})
	if err := a.run(context.Background()); err != nil {
		t.Fatalf("run: %v stderr=%s", err, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "bucket:      bkt") || !strings.Contains(out, "owner:       alice") {
		t.Fatalf("unexpected human output: %q", out)
	}
}

func TestApp_BucketReshard(t *testing.T) {
	h := newGatewayHarness(t, nil)
	ctx := context.Background()
	if _, err := h.store.CreateBucket(ctx, "bkt", "alice", "STANDARD"); err != nil {
		t.Fatalf("create: %v", err)
	}

	var stdout, stderr bytes.Buffer
	a := newApp(&stdout, &stderr, []string{
		"--endpoint", h.ts.URL,
		"--principal", s3api.IAMRootPrincipal,
		"bucket", "reshard", "--bucket", "bkt", "--target", "256",
	})
	if err := a.run(ctx); err != nil {
		t.Fatalf("run: %v stderr=%s", err, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "bucket=bkt") || !strings.Contains(out, "target=256") {
		t.Fatalf("unexpected output: %q", out)
	}
	got, _ := h.store.GetBucket(ctx, "bkt")
	if got.ShardCount != 256 {
		t.Fatalf("post-reshard shard count: %d", got.ShardCount)
	}
}

func TestApp_UnknownCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	a := newApp(&stdout, &stderr, []string{"--endpoint", "http://nowhere", "fake", "subcmd"})
	if err := a.run(context.Background()); err == nil {
		t.Fatalf("expected usage error")
	}
}
