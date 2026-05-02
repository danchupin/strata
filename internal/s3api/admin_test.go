package s3api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/s3api"
)

// TestAdmin_UnknownPathReturnsNotImplemented exercises the dispatch fallthrough
// so /admin/<unknown> doesn't accidentally route into the bucket handler.
func TestAdmin_UnknownPathReturnsNotImplemented(t *testing.T) {
	h := newNotifyHarness(t)
	resp := h.doString("POST", "/admin/nope", "", testPrincipalHeader, s3api.IAMRootPrincipal)
	h.mustStatus(resp, http.StatusNotImplemented)
	resp.Body.Close()
}

func TestAdmin_AnonymousDenied(t *testing.T) {
	h := newNotifyHarness(t)
	for _, path := range []string{"/admin/lifecycle/tick", "/admin/gc/drain", "/admin/sse/rotate", "/admin/replicate/retry?bucket=bkt", "/admin/bucket/inspect?bucket=bkt", "/admin/bucket/reshard?bucket=bkt&target=128"} {
		method := http.MethodPost
		if strings.HasPrefix(path, "/admin/bucket/inspect") {
			method = http.MethodGet
		}
		resp := h.doString(method, path, "")
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("anonymous path=%s status=%d want 403", path, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

func TestAdmin_NonRootDenied(t *testing.T) {
	h := newNotifyHarness(t)
	resp := h.doString("POST", "/admin/lifecycle/tick", "", testPrincipalHeader, "alice")
	h.mustStatus(resp, http.StatusForbidden)
	resp.Body.Close()
}

func TestAdmin_PresignedDenied(t *testing.T) {
	h := newNotifyHarness(t)
	resp := h.doString("POST", "/admin/lifecycle/tick?X-Amz-Signature=abc", "", testPrincipalHeader, s3api.IAMRootPrincipal)
	h.mustStatus(resp, http.StatusForbidden)
	resp.Body.Close()
}

func TestAdmin_LifecycleTick_OK(t *testing.T) {
	h := newNotifyHarness(t)
	resp := h.doString("POST", "/admin/lifecycle/tick", "", testPrincipalHeader, s3api.IAMRootPrincipal)
	h.mustStatus(resp, http.StatusOK)
	if got := resp.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("content-type=%q", got)
	}
	var body struct {
		OK         bool  `json:"ok"`
		DurationMs int64 `json:"duration_ms"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp.Body.Close()
	if !body.OK {
		t.Fatalf("not ok: %+v", body)
	}
}

func TestAdmin_GCDrain_OK(t *testing.T) {
	h := newNotifyHarness(t)
	resp := h.doString("POST", "/admin/gc/drain", "", testPrincipalHeader, s3api.IAMRootPrincipal)
	h.mustStatus(resp, http.StatusOK)
	var body struct {
		OK      bool `json:"ok"`
		Drained int  `json:"drained"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp.Body.Close()
	if !body.OK || body.Drained != 0 {
		t.Fatalf("body=%+v", body)
	}
}

func TestAdmin_SSERotate_NoProvider(t *testing.T) {
	// notifyHarness wires the default harnessMasterProvider (single key, not a
	// rotation provider) so this path returns 400.
	h := newNotifyHarness(t)
	resp := h.doString("POST", "/admin/sse/rotate", "", testPrincipalHeader, s3api.IAMRootPrincipal)
	h.mustStatus(resp, http.StatusBadRequest)
	resp.Body.Close()
}

func TestAdmin_BucketInspect_RoundTrip(t *testing.T) {
	h := newNotifyHarness(t)
	ctx := context.Background()
	if _, err := h.store.CreateBucket(ctx, "bkt", "alice", "STANDARD"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := h.store.SetBucketVersioning(ctx, "bkt", meta.VersioningEnabled); err != nil {
		t.Fatalf("versioning: %v", err)
	}
	b, _ := h.store.GetBucket(ctx, "bkt")
	if err := h.store.SetBucketCORS(ctx, b.ID, []byte(`<CORSConfiguration></CORSConfiguration>`)); err != nil {
		t.Fatalf("cors: %v", err)
	}
	if err := h.store.SetBucketPolicy(ctx, b.ID, []byte(`{"Version":"2012-10-17"}`)); err != nil {
		t.Fatalf("policy: %v", err)
	}

	resp := h.doString("GET", "/admin/bucket/inspect?bucket=bkt", "", testPrincipalHeader, s3api.IAMRootPrincipal)
	h.mustStatus(resp, http.StatusOK)
	var body struct {
		Name       string                     `json:"name"`
		Owner      string                     `json:"owner"`
		Versioning string                     `json:"versioning"`
		Configs    map[string]json.RawMessage `json:"configs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp.Body.Close()
	if body.Name != "bkt" || body.Owner != "alice" || body.Versioning != meta.VersioningEnabled {
		t.Fatalf("body=%+v", body)
	}
	if _, ok := body.Configs["cors"]; !ok {
		t.Fatalf("cors missing: %+v", body.Configs)
	}
	if raw, ok := body.Configs["policy"]; !ok || string(raw) != `{"Version":"2012-10-17"}` {
		t.Fatalf("policy json passthrough failed: %+v", body.Configs)
	}
}

func TestAdmin_BucketInspect_UnknownBucket(t *testing.T) {
	h := newNotifyHarness(t)
	resp := h.doString("GET", "/admin/bucket/inspect?bucket=nope", "", testPrincipalHeader, s3api.IAMRootPrincipal)
	h.mustStatus(resp, http.StatusNotFound)
	resp.Body.Close()
}

func TestAdmin_BucketInspect_MissingParam(t *testing.T) {
	h := newNotifyHarness(t)
	resp := h.doString("GET", "/admin/bucket/inspect", "", testPrincipalHeader, s3api.IAMRootPrincipal)
	h.mustStatus(resp, http.StatusBadRequest)
	resp.Body.Close()
}

func TestAdmin_ReplicateRetry_NoFailedRows(t *testing.T) {
	h := newNotifyHarness(t)
	if _, err := h.store.CreateBucket(context.Background(), "bkt", "alice", "STANDARD"); err != nil {
		t.Fatalf("create: %v", err)
	}
	resp := h.doString("POST", "/admin/replicate/retry?bucket=bkt", "", testPrincipalHeader, s3api.IAMRootPrincipal)
	h.mustStatus(resp, http.StatusOK)
	var body struct {
		OK       bool   `json:"ok"`
		Bucket   string `json:"bucket"`
		Scanned  int    `json:"scanned"`
		Requeued int    `json:"requeued"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp.Body.Close()
	if !body.OK || body.Bucket != "bkt" || body.Requeued != 0 {
		t.Fatalf("body=%+v", body)
	}
}

func TestAdmin_ReplicateRetry_RequeuesFailed(t *testing.T) {
	h := newNotifyHarness(t)
	ctx := context.Background()
	if _, err := h.store.CreateBucket(ctx, "src", s3api.IAMRootPrincipal, "STANDARD"); err != nil {
		t.Fatalf("create src: %v", err)
	}
	if err := h.store.SetBucketVersioning(ctx, "src", meta.VersioningEnabled); err != nil {
		t.Fatalf("versioning src: %v", err)
	}
	src, _ := h.store.GetBucket(ctx, "src")
	repCfg := []byte(`<ReplicationConfiguration>
<Role>arn:aws:iam::strata:role/repl</Role>
<Rule><ID>r1</ID><Status>Enabled</Status><Prefix></Prefix>
<Destination><Bucket>dst</Bucket></Destination></Rule>
</ReplicationConfiguration>`)
	if err := h.store.SetBucketReplication(ctx, src.ID, repCfg); err != nil {
		t.Fatalf("set replication: %v", err)
	}

	// PUT an object as the IAM root so the harness records it.
	resp := h.doString("PUT", "/src/key1", "hello", testPrincipalHeader, s3api.IAMRootPrincipal)
	h.mustStatus(resp, http.StatusOK)
	resp.Body.Close()

	// Drain whatever PUT enqueued so the retry path operates from a blank slate.
	if _, err := h.store.ListPendingReplications(ctx, src.ID, 100); err != nil {
		t.Fatalf("list pending pre: %v", err)
	}
	pending, _ := h.store.ListPendingReplications(ctx, src.ID, 100)
	for _, evt := range pending {
		_ = h.store.AckReplication(ctx, evt)
	}

	// Find the object's version id and mark it FAILED so the retry handler
	// picks it up.
	versions, err := h.store.ListObjectVersions(ctx, src.ID, meta.ListOptions{Limit: 10})
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	if len(versions.Versions) == 0 {
		t.Fatalf("no versions after PUT")
	}
	target := versions.Versions[0]
	if err := h.store.SetObjectReplicationStatus(ctx, src.ID, target.Key, target.VersionID, "FAILED"); err != nil {
		t.Fatalf("mark failed: %v", err)
	}

	resp = h.doString("POST", "/admin/replicate/retry?bucket=src", "", testPrincipalHeader, s3api.IAMRootPrincipal)
	h.mustStatus(resp, http.StatusOK)
	var body struct {
		Scanned  int `json:"scanned"`
		Requeued int `json:"requeued"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp.Body.Close()
	if body.Requeued != 1 {
		t.Fatalf("expected 1 requeued, got %+v", body)
	}
	pending, err = h.store.ListPendingReplications(ctx, src.ID, 100)
	if err != nil {
		t.Fatalf("list after retry: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("expected 1 enqueued event, got %d", len(pending))
	}
}

func TestAdmin_LifecycleTick_MethodGuard(t *testing.T) {
	h := newNotifyHarness(t)
	resp := h.doString("GET", "/admin/lifecycle/tick", "", testPrincipalHeader, s3api.IAMRootPrincipal)
	h.mustStatus(resp, http.StatusMethodNotAllowed)
	resp.Body.Close()
}

func TestAdmin_IAMRotateAccessKey_RoundTrip(t *testing.T) {
	h := newNotifyHarness(t)
	ctx := context.Background()
	if err := h.store.CreateIAMUser(ctx, &meta.IAMUser{UserName: "alice"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	resp := iamCall(t, h.testHarness, "CreateAccessKey", s3api.IAMRootPrincipal, "UserName", "alice")
	h.mustStatus(resp, http.StatusOK)
	var created createAccessKeyResp
	decodeXML(t, resp.Body, &created)
	resp.Body.Close()
	oldID := created.Result.AccessKey.AccessKeyID

	resp = iamCall(t, h.testHarness, "RotateAccessKey", s3api.IAMRootPrincipal, "AccessKeyId", oldID)
	h.mustStatus(resp, http.StatusOK)
	var rotated createAccessKeyResp
	decodeXML(t, resp.Body, &rotated)
	resp.Body.Close()
	if rotated.Result.AccessKey.AccessKeyID == oldID {
		t.Fatalf("rotation kept same id %q", oldID)
	}
	if rotated.Result.AccessKey.UserName != "alice" {
		t.Fatalf("user=%q", rotated.Result.AccessKey.UserName)
	}
	if _, err := h.store.GetIAMAccessKey(ctx, oldID); err == nil {
		t.Fatalf("old key still exists after rotation")
	}
}

func TestAdmin_IAMRotateAccessKey_UnknownKey(t *testing.T) {
	h := newNotifyHarness(t)
	resp := iamCall(t, h.testHarness, "RotateAccessKey", s3api.IAMRootPrincipal, "AccessKeyId", "AKIAGHOST")
	h.mustStatus(resp, http.StatusNotFound)
	resp.Body.Close()
}

func TestAdmin_IAMRotateAccessKey_MissingParam(t *testing.T) {
	h := newNotifyHarness(t)
	resp := iamCall(t, h.testHarness, "RotateAccessKey", s3api.IAMRootPrincipal)
	h.mustStatus(resp, http.StatusBadRequest)
	resp.Body.Close()
}

func TestAdmin_BucketReshard_RoundTrip(t *testing.T) {
	h := newNotifyHarness(t)
	ctx := context.Background()
	if _, err := h.store.CreateBucket(ctx, "bkt", "alice", "STANDARD"); err != nil {
		t.Fatalf("create: %v", err)
	}
	b, _ := h.store.GetBucket(ctx, "bkt")
	source := b.ShardCount
	target := source * 4

	resp := h.doString("POST", "/admin/bucket/reshard?bucket=bkt&target="+itoa(target), "", testPrincipalHeader, s3api.IAMRootPrincipal)
	h.mustStatus(resp, http.StatusOK)
	var body struct {
		OK            bool   `json:"ok"`
		Bucket        string `json:"bucket"`
		Source        int    `json:"source"`
		Target        int    `json:"target"`
		JobsCompleted int    `json:"jobs_completed"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp.Body.Close()
	if !body.OK || body.Bucket != "bkt" || body.Source != source || body.Target != target || body.JobsCompleted != 1 {
		t.Fatalf("body=%+v", body)
	}
	got, _ := h.store.GetBucket(ctx, "bkt")
	if got.ShardCount != target {
		t.Fatalf("post-call shard count=%d want %d", got.ShardCount, target)
	}
}

func TestAdmin_BucketReshard_BadTarget(t *testing.T) {
	h := newNotifyHarness(t)
	ctx := context.Background()
	if _, err := h.store.CreateBucket(ctx, "bkt", "alice", "STANDARD"); err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, q := range []string{"bucket=bkt&target=0", "bucket=bkt&target=3", "bucket=bkt", "target=128"} {
		resp := h.doString("POST", "/admin/bucket/reshard?"+q, "", testPrincipalHeader, s3api.IAMRootPrincipal)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("q=%s status=%d", q, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

func TestAdmin_BucketReshard_UnknownBucket(t *testing.T) {
	h := newNotifyHarness(t)
	resp := h.doString("POST", "/admin/bucket/reshard?bucket=missing&target=128", "", testPrincipalHeader, s3api.IAMRootPrincipal)
	h.mustStatus(resp, http.StatusNotFound)
	resp.Body.Close()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
