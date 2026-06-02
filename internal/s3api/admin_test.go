package s3api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/danchupin/strata/internal/auth"
	datamem "github.com/danchupin/strata/internal/data/memory"
	"github.com/danchupin/strata/internal/meta"
	metamem "github.com/danchupin/strata/internal/meta/memory"
	"github.com/danchupin/strata/internal/reshard"
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

func TestAdmin_Reconcile_QueueAndStatus(t *testing.T) {
	h := newNotifyHarness(t)

	// Queue a report-policy pass.
	resp := h.doString("POST", "/admin/reconcile?cluster=ceph-a&pool=strata-data&policy=report", "", testPrincipalHeader, s3api.IAMRootPrincipal)
	h.mustStatus(resp, http.StatusAccepted)
	var queued struct {
		OK      bool   `json:"ok"`
		ID      string `json:"id"`
		Cluster string `json:"cluster"`
		Pool    string `json:"pool"`
		Policy  string `json:"policy"`
		State   string `json:"state"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&queued); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp.Body.Close()
	if !queued.OK || queued.ID == "" || queued.State != "queued" ||
		queued.Cluster != "ceph-a" || queued.Pool != "strata-data" || queued.Policy != "report" {
		t.Fatalf("queue round-trip: %+v", queued)
	}

	// Poll its status by id.
	resp = h.doString("GET", "/admin/reconcile?id="+queued.ID, "", testPrincipalHeader, s3api.IAMRootPrincipal)
	h.mustStatus(resp, http.StatusOK)
	var status struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	resp.Body.Close()
	if status.ID != queued.ID {
		t.Fatalf("status id mismatch: %+v", status)
	}
}

func TestAdmin_Reconcile_RejectsBadInput(t *testing.T) {
	h := newNotifyHarness(t)

	// restore policy is an accepted orphan-pass policy (US-002b) -> 202.
	resp := h.doString("POST", "/admin/reconcile?cluster=ceph-a&pool=strata-data&policy=restore", "", testPrincipalHeader, s3api.IAMRootPrincipal)
	h.mustStatus(resp, http.StatusAccepted)
	resp.Body.Close()

	// Bogus policy -> 400.
	resp = h.doString("POST", "/admin/reconcile?cluster=ceph-a&pool=strata-data&policy=bogus", "", testPrincipalHeader, s3api.IAMRootPrincipal)
	h.mustStatus(resp, http.StatusBadRequest)
	resp.Body.Close()

	// Missing pool -> 400.
	resp = h.doString("POST", "/admin/reconcile?cluster=ceph-a", "", testPrincipalHeader, s3api.IAMRootPrincipal)
	h.mustStatus(resp, http.StatusBadRequest)
	resp.Body.Close()

	// Unknown job id -> 404.
	resp = h.doString("GET", "/admin/reconcile?id=does-not-exist", "", testPrincipalHeader, s3api.IAMRootPrincipal)
	h.mustStatus(resp, http.StatusNotFound)
	resp.Body.Close()

	// quarantine policy on an orphan (no-bucket) job -> 400 (dangling-only).
	resp = h.doString("POST", "/admin/reconcile?cluster=ceph-a&pool=strata-data&policy=quarantine", "", testPrincipalHeader, s3api.IAMRootPrincipal)
	h.mustStatus(resp, http.StatusBadRequest)
	resp.Body.Close()
}

// TestAdmin_Reconcile_DanglingQueue queues a meta->data dangling pass for a
// bucket (US-003) and asserts the bucket-scoped job round-trips with the
// quarantine policy.
func TestAdmin_Reconcile_DanglingQueue(t *testing.T) {
	h := newNotifyHarness(t)

	// Create the bucket the dangling pass will scan.
	h.mustStatus(h.doString("PUT", "/dangle", "", testPrincipalHeader, s3api.IAMRootPrincipal), http.StatusOK)

	resp := h.doString("POST", "/admin/reconcile?bucket=dangle&policy=quarantine", "", testPrincipalHeader, s3api.IAMRootPrincipal)
	h.mustStatus(resp, http.StatusAccepted)
	var queued struct {
		OK     bool   `json:"ok"`
		ID     string `json:"id"`
		Bucket string `json:"bucket"`
		Policy string `json:"policy"`
		State  string `json:"state"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&queued); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp.Body.Close()
	if !queued.OK || queued.ID == "" || queued.State != "queued" ||
		queued.Bucket == "" || queued.Policy != "quarantine" {
		t.Fatalf("dangling queue round-trip: %+v", queued)
	}

	// A dangling pass over a nonexistent bucket -> 404.
	resp = h.doString("POST", "/admin/reconcile?bucket=ghostbucket&policy=report", "", testPrincipalHeader, s3api.IAMRootPrincipal)
	h.mustStatus(resp, http.StatusNotFound)
	resp.Body.Close()
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

// TestAdmin_BucketReshard_TriggerIsAsync proves the US-005 split: the POST
// trigger queues a job and returns 202 immediately WITHOUT driving the worker
// inline — the bucket's active shard count must NOT have flipped on return
// (the leader-elected reshard worker does the migration out-of-band). A
// separate GET progress read then reports the queued job, and once the worker
// runs the progress flips to state=idle at the target count.
func TestAdmin_BucketReshard_TriggerIsAsync(t *testing.T) {
	h := newNotifyHarness(t)
	ctx := context.Background()
	if _, err := h.store.CreateBucket(ctx, "bkt", "alice", "STANDARD"); err != nil {
		t.Fatalf("create: %v", err)
	}
	b, _ := h.store.GetBucket(ctx, "bkt")
	source := b.ShardCount
	target := source * 4

	resp := h.doString("POST", "/admin/bucket/reshard?bucket=bkt&target="+itoa(target), "", testPrincipalHeader, s3api.IAMRootPrincipal)
	h.mustStatus(resp, http.StatusAccepted)
	var body struct {
		OK     bool   `json:"ok"`
		Bucket string `json:"bucket"`
		Source int    `json:"source"`
		Target int    `json:"target"`
		State  string `json:"state"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp.Body.Close()
	if !body.OK || body.Bucket != "bkt" || body.Source != source || body.Target != target || body.State != "queued" {
		t.Fatalf("trigger body=%+v", body)
	}
	// Async: the active count must still be the source — the trigger did NOT
	// drive the worker inline. The job is queued and the target is stamped.
	got, _ := h.store.GetBucket(ctx, "bkt")
	if got.ShardCount != source {
		t.Fatalf("trigger flipped shard count inline: got %d want %d (must stay until the worker completes)", got.ShardCount, source)
	}
	if got.TargetShardCount != target {
		t.Fatalf("trigger did not stamp target: got %d want %d", got.TargetShardCount, target)
	}

	// Progress read while queued.
	resp = h.doString("GET", "/admin/bucket/reshard?bucket=bkt", "", testPrincipalHeader, s3api.IAMRootPrincipal)
	h.mustStatus(resp, http.StatusOK)
	var prog struct {
		State      string `json:"state"`
		Source     int    `json:"source"`
		Target     int    `json:"target"`
		ShardCount int    `json:"shard_count"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&prog); err != nil {
		t.Fatalf("decode progress: %v", err)
	}
	resp.Body.Close()
	if prog.State != "queued" || prog.Target != target {
		t.Fatalf("progress (queued) body=%+v", prog)
	}

	// Drive the worker the way the registered background worker would, then the
	// progress read must report the completed (idle) state at the target count.
	worker, err := reshard.New(reshard.Config{Meta: h.store})
	if err != nil {
		t.Fatalf("worker new: %v", err)
	}
	if _, err := worker.RunOnce(ctx); err != nil {
		t.Fatalf("worker run: %v", err)
	}
	resp = h.doString("GET", "/admin/bucket/reshard?bucket=bkt", "", testPrincipalHeader, s3api.IAMRootPrincipal)
	h.mustStatus(resp, http.StatusOK)
	if err := json.NewDecoder(resp.Body).Decode(&prog); err != nil {
		t.Fatalf("decode progress after: %v", err)
	}
	resp.Body.Close()
	if prog.State != "idle" || prog.ShardCount != target {
		t.Fatalf("progress (after worker) body=%+v want state=idle shard_count=%d", prog, target)
	}
}

// TestAdmin_BucketReshard_InProgressConflict pins the 409 on a second trigger
// while a job is already queued.
func TestAdmin_BucketReshard_InProgressConflict(t *testing.T) {
	h := newNotifyHarness(t)
	ctx := context.Background()
	if _, err := h.store.CreateBucket(ctx, "bkt", "alice", "STANDARD"); err != nil {
		t.Fatalf("create: %v", err)
	}
	resp := h.doString("POST", "/admin/bucket/reshard?bucket=bkt&target=256", "", testPrincipalHeader, s3api.IAMRootPrincipal)
	h.mustStatus(resp, http.StatusAccepted)
	resp.Body.Close()

	resp = h.doString("POST", "/admin/bucket/reshard?bucket=bkt&target=512", "", testPrincipalHeader, s3api.IAMRootPrincipal)
	h.mustStatus(resp, http.StatusConflict)
	resp.Body.Close()
}

// TestAdmin_BucketReshard_StatusIdleWhenNoJob covers the progress read with no
// job in flight: state=idle reports the bucket's current shard count.
func TestAdmin_BucketReshard_StatusIdleWhenNoJob(t *testing.T) {
	h := newNotifyHarness(t)
	ctx := context.Background()
	if _, err := h.store.CreateBucket(ctx, "bkt", "alice", "STANDARD"); err != nil {
		t.Fatalf("create: %v", err)
	}
	b, _ := h.store.GetBucket(ctx, "bkt")

	resp := h.doString("GET", "/admin/bucket/reshard?bucket=bkt", "", testPrincipalHeader, s3api.IAMRootPrincipal)
	h.mustStatus(resp, http.StatusOK)
	var prog struct {
		State      string `json:"state"`
		ShardCount int    `json:"shard_count"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&prog); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp.Body.Close()
	if prog.State != "idle" || prog.ShardCount != b.ShardCount {
		t.Fatalf("body=%+v want state=idle shard_count=%d", prog, b.ShardCount)
	}
}

// TestAdmin_BucketReshard_StatusRunning covers the queued->running transition:
// once the worker advances the LastKey watermark, the progress read reports
// state=running with the watermark.
func TestAdmin_BucketReshard_StatusRunning(t *testing.T) {
	h := newNotifyHarness(t)
	ctx := context.Background()
	if _, err := h.store.CreateBucket(ctx, "bkt", "alice", "STANDARD"); err != nil {
		t.Fatalf("create: %v", err)
	}
	b, _ := h.store.GetBucket(ctx, "bkt")
	job, err := h.store.StartReshard(ctx, b.ID, b.ShardCount*2)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	job.LastKey = "k-042"
	if err := h.store.UpdateReshardJob(ctx, job); err != nil {
		t.Fatalf("update job: %v", err)
	}

	resp := h.doString("GET", "/admin/bucket/reshard?bucket=bkt", "", testPrincipalHeader, s3api.IAMRootPrincipal)
	h.mustStatus(resp, http.StatusOK)
	var prog struct {
		State   string `json:"state"`
		LastKey string `json:"last_key"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&prog); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp.Body.Close()
	if prog.State != "running" || prog.LastKey != "k-042" {
		t.Fatalf("body=%+v want state=running last_key=k-042", prog)
	}
}

// TestAdmin_BucketReshard_TriggerAudited proves the trigger stamps the
// admin:BucketReshard audit override (CLAUDE.md "every new admin write" rule).
func TestAdmin_BucketReshard_TriggerAudited(t *testing.T) {
	store := metamem.New()
	api := s3api.New(datamem.New(), store)
	api.Region = "default"
	api.Master = harnessMasterProvider{}
	rootInjector := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := auth.WithAuth(r.Context(), &auth.AuthInfo{Owner: s3api.IAMRootPrincipal, AccessKey: s3api.IAMRootPrincipal})
		api.ServeHTTP(w, r.WithContext(ctx))
	})
	ts := httptest.NewServer(s3api.NewAuditMiddleware(store, time.Hour, rootInjector))
	t.Cleanup(ts.Close)

	ctx := context.Background()
	if _, err := store.CreateBucket(ctx, "bkt", "alice", "STANDARD"); err != nil {
		t.Fatalf("create: %v", err)
	}
	b, _ := store.GetBucket(ctx, "bkt")

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/admin/bucket/reshard?bucket=bkt&target="+itoa(b.ShardCount*2), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status=%d want 202", resp.StatusCode)
	}
	resp.Body.Close()

	rows, err := store.ListAudit(ctx, b.ID, 100)
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	var found bool
	for _, row := range rows {
		if row.Action == "admin:BucketReshard" {
			found = true
			if row.Resource != "bucket:bkt" {
				t.Fatalf("audit resource=%q want bucket:bkt", row.Resource)
			}
			if row.Principal != s3api.IAMRootPrincipal {
				t.Fatalf("audit principal=%q want %q", row.Principal, s3api.IAMRootPrincipal)
			}
		}
	}
	if !found {
		t.Fatalf("no admin:BucketReshard audit row; rows=%+v", rows)
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
