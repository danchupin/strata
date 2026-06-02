package s3api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/crypto/master"
	"github.com/danchupin/strata/internal/gc"
	"github.com/danchupin/strata/internal/lifecycle"
	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/rewrap"
)

// handleAdmin dispatches the [iam root]-gated /admin/* admin surface (US-034).
// Every admin action is POST except inspect (GET). Same gate semantics as
// /?audit + /?notify-dlq: presigned URLs rejected, only the IAMRootPrincipal
// owner is allowed.
func (s *Server) handleAdmin(w http.ResponseWriter, r *http.Request, sub string) {
	if r.URL.Query().Get("X-Amz-Signature") != "" {
		writeError(w, r, ErrAccessDenied)
		return
	}
	info := auth.FromContext(r.Context())
	if info == nil || info.IsAnonymous || info.Owner != IAMRootPrincipal {
		writeError(w, r, ErrAccessDenied)
		return
	}
	switch sub {
	case "lifecycle/tick":
		if r.Method != http.MethodPost {
			writeError(w, r, errAdminMethodNotAllowed)
			return
		}
		s.adminLifecycleTick(w, r)
	case "gc/drain":
		if r.Method != http.MethodPost {
			writeError(w, r, errAdminMethodNotAllowed)
			return
		}
		s.adminGCDrain(w, r)
	case "sse/rotate":
		if r.Method != http.MethodPost {
			writeError(w, r, errAdminMethodNotAllowed)
			return
		}
		s.adminSSERotate(w, r)
	case "replicate/retry":
		if r.Method != http.MethodPost {
			writeError(w, r, errAdminMethodNotAllowed)
			return
		}
		s.adminReplicateRetry(w, r)
	case "bucket/inspect":
		if r.Method != http.MethodGet {
			writeError(w, r, errAdminMethodNotAllowed)
			return
		}
		s.adminBucketInspect(w, r)
	case "bucket/reshard":
		switch r.Method {
		case http.MethodPost:
			s.adminBucketReshard(w, r)
		case http.MethodGet:
			s.adminBucketReshardStatus(w, r)
		default:
			writeError(w, r, errAdminMethodNotAllowed)
		}
	case "reconcile":
		switch r.Method {
		case http.MethodPost:
			s.adminReconcile(w, r)
		case http.MethodGet:
			s.adminReconcileStatus(w, r)
		default:
			writeError(w, r, errAdminMethodNotAllowed)
		}
	default:
		writeError(w, r, ErrNotImplemented)
	}
}

var errAdminMethodNotAllowed = APIError{Code: "MethodNotAllowed", Message: "The specified method is not allowed against this resource", Status: http.StatusMethodNotAllowed}

type adminLifecycleResponse struct {
	OK         bool   `json:"ok"`
	DurationMs int64  `json:"duration_ms"`
	Error      string `json:"error,omitempty"`
}

func (s *Server) adminLifecycleTick(w http.ResponseWriter, r *http.Request) {
	worker := &lifecycle.Worker{
		Meta:    s.Meta,
		Data:    s.Data,
		Region:  s.Region,
		AgeUnit: 24 * time.Hour,
	}
	start := time.Now()
	err := worker.RunOnce(r.Context())
	resp := adminLifecycleResponse{OK: err == nil, DurationMs: time.Since(start).Milliseconds()}
	if err != nil {
		resp.Error = err.Error()
		writeAdminJSON(w, http.StatusInternalServerError, resp)
		return
	}
	writeAdminJSON(w, http.StatusOK, resp)
}

type adminGCResponse struct {
	OK         bool  `json:"ok"`
	Drained    int   `json:"drained"`
	DurationMs int64 `json:"duration_ms"`
}

func (s *Server) adminGCDrain(w http.ResponseWriter, r *http.Request) {
	worker := &gc.Worker{
		Meta:   s.Meta,
		Data:   s.Data,
		Region: s.Region,
	}
	start := time.Now()
	drained := worker.RunOnce(r.Context())
	writeAdminJSON(w, http.StatusOK, adminGCResponse{
		OK:         true,
		Drained:    drained,
		DurationMs: time.Since(start).Milliseconds(),
	})
}

type adminSSERotateResponse struct {
	OK               bool   `json:"ok"`
	ActiveID         string `json:"active_id"`
	BucketsScanned   int    `json:"buckets_scanned"`
	BucketsSkipped   int    `json:"buckets_skipped"`
	ObjectsScanned   int    `json:"objects_scanned"`
	ObjectsRewrapped int    `json:"objects_rewrapped"`
	UploadsScanned   int    `json:"uploads_scanned"`
	UploadsRewrapped int    `json:"uploads_rewrapped"`
	DurationMs       int64  `json:"duration_ms"`
	Error            string `json:"error,omitempty"`
}

func (s *Server) adminSSERotate(w http.ResponseWriter, r *http.Request) {
	rot, ok := s.Master.(*master.RotationProvider)
	if !ok || rot == nil {
		writeError(w, r, APIError{
			Code:    "InvalidRequest",
			Message: "sse rotate requires STRATA_SSE_MASTER_KEYS rotation provider",
			Status:  http.StatusBadRequest,
		})
		return
	}
	worker, err := rewrap.New(rewrap.Config{Meta: s.Meta, Provider: rot})
	if err != nil {
		writeError(w, r, ErrInternal)
		return
	}
	start := time.Now()
	stats, runErr := worker.Run(r.Context())
	resp := adminSSERotateResponse{
		OK:               runErr == nil,
		ActiveID:         rot.ActiveID(),
		BucketsScanned:   stats.BucketsScanned,
		BucketsSkipped:   stats.BucketsSkipped,
		ObjectsScanned:   stats.ObjectsScanned,
		ObjectsRewrapped: stats.ObjectsRewrapped,
		UploadsScanned:   stats.UploadsScanned,
		UploadsRewrapped: stats.UploadsRewrapped,
		DurationMs:       time.Since(start).Milliseconds(),
	}
	status := http.StatusOK
	if runErr != nil {
		resp.Error = runErr.Error()
		status = http.StatusInternalServerError
	}
	writeAdminJSON(w, status, resp)
}

type adminReplicateRetryResponse struct {
	OK       bool   `json:"ok"`
	Bucket   string `json:"bucket"`
	Scanned  int    `json:"scanned"`
	Requeued int    `json:"requeued"`
	Error    string `json:"error,omitempty"`
}

// adminReplicateRetry walks every version of a bucket whose replication-status
// is FAILED, re-emits replication events through the configured rules and
// resets the source's status to PENDING so a subsequent replicator pass picks
// it up. Bucket without replication configuration yields a no-op (scanned=0).
func (s *Server) adminReplicateRetry(w http.ResponseWriter, r *http.Request) {
	bucketName := r.URL.Query().Get("bucket")
	if bucketName == "" {
		writeError(w, r, ErrInvalidArgument)
		return
	}
	b, err := s.Meta.GetBucket(r.Context(), bucketName)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	scanned, requeued, runErr := s.replicateRetry(r, b)
	resp := adminReplicateRetryResponse{
		OK:       runErr == nil,
		Bucket:   bucketName,
		Scanned:  scanned,
		Requeued: requeued,
	}
	status := http.StatusOK
	if runErr != nil {
		resp.Error = runErr.Error()
		status = http.StatusInternalServerError
	}
	writeAdminJSON(w, status, resp)
}

func (s *Server) replicateRetry(r *http.Request, b *meta.Bucket) (scanned, requeued int, err error) {
	opts := meta.ListOptions{Limit: 1000}
	for {
		res, lerr := s.Meta.ListObjectVersions(r.Context(), b.ID, opts)
		if lerr != nil {
			return scanned, requeued, lerr
		}
		for _, v := range res.Versions {
			scanned++
			if v.ReplicationStatus != "FAILED" || v.IsDeleteMarker {
				continue
			}
			tags, _ := s.Meta.GetObjectTags(r.Context(), b.ID, v.Key, v.VersionID)
			status := s.emitReplicationEvent(r, b, replicationEventDetails{
				EventName: "ObjectReplicationRetry",
				Key:       v.Key,
				VersionID: v.VersionID,
				Tags:      tags,
			})
			if status == "" {
				continue
			}
			if err := s.Meta.SetObjectReplicationStatus(r.Context(), b.ID, v.Key, v.VersionID, status); err != nil {
				return scanned, requeued, err
			}
			requeued++
		}
		if !res.Truncated {
			return scanned, requeued, nil
		}
		opts.Marker = res.NextKeyMarker
	}
}

type adminBucketInspectResponse struct {
	Name              string                     `json:"name"`
	ID                string                     `json:"id"`
	Owner             string                     `json:"owner"`
	CreatedAt         time.Time                  `json:"created_at"`
	DefaultClass      string                     `json:"default_class"`
	Versioning        string                     `json:"versioning,omitempty"`
	ACL               string                     `json:"acl,omitempty"`
	ObjectLockEnabled bool                       `json:"object_lock_enabled"`
	Region            string                     `json:"region,omitempty"`
	MfaDelete         string                     `json:"mfa_delete,omitempty"`
	Configs           map[string]json.RawMessage `json:"configs,omitempty"`
}

func (s *Server) adminBucketInspect(w http.ResponseWriter, r *http.Request) {
	bucketName := r.URL.Query().Get("bucket")
	if bucketName == "" {
		writeError(w, r, ErrInvalidArgument)
		return
	}
	b, err := s.Meta.GetBucket(r.Context(), bucketName)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	resp := adminBucketInspectResponse{
		Name:              b.Name,
		ID:                b.ID.String(),
		Owner:             b.Owner,
		CreatedAt:         b.CreatedAt.UTC(),
		DefaultClass:      b.DefaultClass,
		Versioning:        b.Versioning,
		ACL:               b.ACL,
		ObjectLockEnabled: b.ObjectLockEnabled,
		Region:            b.Region,
		MfaDelete:         b.MfaDelete,
		Configs:           map[string]json.RawMessage{},
	}
	for name, getter := range bucketConfigGetters(s, b.ID) {
		blob, err := getter(r.Context())
		if err != nil {
			continue
		}
		if len(blob) == 0 {
			continue
		}
		resp.Configs[name] = encodeConfigBlob(blob)
	}
	writeAdminJSON(w, http.StatusOK, resp)
}

// bucketConfigGetters returns one getter per bucket-scoped meta blob. Each
// getter is best-effort — a "no such config" error from the backend is
// silently skipped by the caller.
func bucketConfigGetters(s *Server, id uuid.UUID) map[string]func(context.Context) ([]byte, error) {
	return map[string]func(context.Context) ([]byte, error){
		"lifecycle":           func(ctx context.Context) ([]byte, error) { return s.Meta.GetBucketLifecycle(ctx, id) },
		"cors":                func(ctx context.Context) ([]byte, error) { return s.Meta.GetBucketCORS(ctx, id) },
		"policy":              func(ctx context.Context) ([]byte, error) { return s.Meta.GetBucketPolicy(ctx, id) },
		"public_access_block": func(ctx context.Context) ([]byte, error) { return s.Meta.GetBucketPublicAccessBlock(ctx, id) },
		"ownership_controls":  func(ctx context.Context) ([]byte, error) { return s.Meta.GetBucketOwnershipControls(ctx, id) },
		"encryption":          func(ctx context.Context) ([]byte, error) { return s.Meta.GetBucketEncryption(ctx, id) },
		"object_lock":         func(ctx context.Context) ([]byte, error) { return s.Meta.GetBucketObjectLockConfig(ctx, id) },
		"notification":        func(ctx context.Context) ([]byte, error) { return s.Meta.GetBucketNotificationConfig(ctx, id) },
		"website":             func(ctx context.Context) ([]byte, error) { return s.Meta.GetBucketWebsite(ctx, id) },
		"replication":         func(ctx context.Context) ([]byte, error) { return s.Meta.GetBucketReplication(ctx, id) },
		"logging":             func(ctx context.Context) ([]byte, error) { return s.Meta.GetBucketLogging(ctx, id) },
		"tagging":             func(ctx context.Context) ([]byte, error) { return s.Meta.GetBucketTagging(ctx, id) },
	}
}

// encodeConfigBlob turns a stored XML/JSON config blob into a JSON value.
// JSON blobs are passed through; everything else is wrapped as a JSON string
// so the inspect response is always parseable JSON.
func encodeConfigBlob(blob []byte) json.RawMessage {
	if json.Valid(blob) {
		return json.RawMessage(blob)
	}
	quoted, _ := json.Marshal(string(blob))
	return json.RawMessage(quoted)
}

// adminBucketReshardResponse is the async-reshard payload. State is one of
// "queued" (job created, worker has not started moving rows), "running" (the
// worker has advanced the LastKey watermark at least once), or "idle" (no job
// in flight — the bucket already sits at ShardCount).
type adminBucketReshardResponse struct {
	OK         bool   `json:"ok"`
	Bucket     string `json:"bucket"`
	Source     int    `json:"source"`
	Target     int    `json:"target"`
	State      string `json:"state"`
	LastKey    string `json:"last_key,omitempty"`
	ShardCount int    `json:"shard_count,omitempty"`
	Error      string `json:"error,omitempty"`
}

// adminBucketReshard queues an online shard-resize for the named bucket and
// returns immediately (202 Accepted) with a job descriptor. The physical row
// migration is driven asynchronously by the leader-elected `reshard` worker
// (cmd/strata/workers/reshard.go) — a large bucket's 64->128 reshard must not
// block the HTTP request for minutes. Operators poll progress via
// GET /admin/bucket/reshard?bucket=<name> (adminBucketReshardStatus). Enable
// the worker with STRATA_WORKERS=...,reshard on at least one replica.
func (s *Server) adminBucketReshard(w http.ResponseWriter, r *http.Request) {
	bucketName := r.URL.Query().Get("bucket")
	if bucketName == "" {
		writeError(w, r, ErrInvalidArgument)
		return
	}
	target, err := strconv.Atoi(r.URL.Query().Get("target"))
	if err != nil || !meta.IsValidShardCount(target) {
		writeError(w, r, APIError{
			Code:    "InvalidArgument",
			Message: "target must be a positive power of two",
			Status:  http.StatusBadRequest,
		})
		return
	}
	b, err := s.Meta.GetBucket(r.Context(), bucketName)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	job, err := s.Meta.StartReshard(r.Context(), b.ID, target)
	if err != nil {
		switch err {
		case meta.ErrReshardInProgress:
			writeError(w, r, APIError{Code: "OperationAborted", Message: err.Error(), Status: http.StatusConflict})
		case meta.ErrReshardInvalidTarget:
			writeError(w, r, APIError{Code: "InvalidArgument", Message: err.Error(), Status: http.StatusBadRequest})
		default:
			mapMetaErr(w, r, err)
		}
		return
	}
	SetAuditOverride(r.Context(), "admin:BucketReshard", "bucket:"+bucketName, bucketName, principalFromContext(r))
	writeAdminJSON(w, http.StatusAccepted, adminBucketReshardResponse{
		OK:     true,
		Bucket: bucketName,
		Source: job.Source,
		Target: job.Target,
		State:  "queued",
	})
}

// adminBucketReshardStatus reads the queued/running reshard job for a bucket
// so operators can watch a long-running reshard converge. When no job is in
// flight it reports state="idle" with the bucket's current ShardCount — the
// signal that an async reshard has completed. Backed by meta.GetReshardJob on
// every backend (memory/TiKV report the no-op job between Start and the
// worker's immediate-complete pass).
func (s *Server) adminBucketReshardStatus(w http.ResponseWriter, r *http.Request) {
	bucketName := r.URL.Query().Get("bucket")
	if bucketName == "" {
		writeError(w, r, ErrInvalidArgument)
		return
	}
	b, err := s.Meta.GetBucket(r.Context(), bucketName)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	job, err := s.Meta.GetReshardJob(r.Context(), b.ID)
	if errors.Is(err, meta.ErrReshardNotFound) {
		writeAdminJSON(w, http.StatusOK, adminBucketReshardResponse{
			OK:         true,
			Bucket:     bucketName,
			State:      "idle",
			ShardCount: b.ShardCount,
		})
		return
	}
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	state := "queued"
	if job.LastKey != "" {
		state = "running"
	}
	writeAdminJSON(w, http.StatusOK, adminBucketReshardResponse{
		OK:      true,
		Bucket:  bucketName,
		Source:  job.Source,
		Target:  job.Target,
		State:   state,
		LastKey: job.LastKey,
	})
}

// adminReconcileResponse is the reconcile-job payload returned by both the
// POST (queue) and GET (status) handlers (US-002 metadata-data-reconcile).
type adminReconcileResponse struct {
	OK            bool   `json:"ok"`
	ID            string `json:"id"`
	Cluster       string `json:"cluster"`
	Pool          string `json:"pool"`
	Namespace     string `json:"namespace,omitempty"`
	Policy        string `json:"policy"`
	State         string `json:"state"`
	Cursor        string `json:"cursor,omitempty"`
	Scanned       int64  `json:"scanned"`
	OrphansFound  int64  `json:"orphans_found"`
	OrphansGC     int64  `json:"orphans_gc"`
	OrphansReport int64  `json:"orphans_report"`
	AbsentBackref int64  `json:"absent_backref"`
	Errors        int64  `json:"errors"`
	Message       string `json:"message,omitempty"`
}

// adminReconcile queues a data-tier reconcile pass over a (cluster, pool,
// namespace) scope and returns immediately (202 Accepted) with a job id. The
// pool walk + orphan resolution is driven asynchronously by the leader-elected
// `reconcile` worker (cmd/strata/workers/reconcile.go) — a live-cluster scan
// must not block the HTTP request. Operators poll progress via
// GET /admin/reconcile?id=<id>. Enable the worker with
// STRATA_WORKERS=...,reconcile on at least one replica.
//
// Query params: cluster (required), pool (required), namespace (optional —
// default namespace; \x01 for all namespaces), policy (report|gc, default
// report). restore is rejected (US-002b).
func (s *Server) adminReconcile(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	cluster := q.Get("cluster")
	pool := q.Get("pool")
	if cluster == "" || pool == "" {
		writeError(w, r, APIError{
			Code:    "InvalidArgument",
			Message: "cluster and pool are required",
			Status:  http.StatusBadRequest,
		})
		return
	}
	namespace := q.Get("namespace")
	policy := q.Get("policy")
	if policy == "" {
		policy = meta.ReconcilePolicyReport
	}
	job, err := s.Meta.StartReconcile(r.Context(), cluster, pool, namespace, policy)
	if err != nil {
		if errors.Is(err, meta.ErrReconcileInvalidPolicy) {
			writeError(w, r, APIError{Code: "InvalidArgument", Message: err.Error(), Status: http.StatusBadRequest})
			return
		}
		mapMetaErr(w, r, err)
		return
	}
	SetAuditOverride(r.Context(), "admin:Reconcile", "cluster:"+cluster+"/"+pool, "", principalFromContext(r))
	writeAdminJSON(w, http.StatusAccepted, reconcileResponse(job))
}

// adminReconcileStatus reads a reconcile job by id so operators can watch a
// pass converge and read the post-run orphan/dangling summary.
func (s *Server) adminReconcileStatus(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		writeError(w, r, ErrInvalidArgument)
		return
	}
	job, err := s.Meta.GetReconcileJob(r.Context(), id)
	if errors.Is(err, meta.ErrReconcileNotFound) {
		writeError(w, r, APIError{Code: "NoSuchReconcileJob", Message: "no reconcile job with that id", Status: http.StatusNotFound})
		return
	}
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	writeAdminJSON(w, http.StatusOK, reconcileResponse(job))
}

func reconcileResponse(job *meta.ReconcileJob) adminReconcileResponse {
	return adminReconcileResponse{
		OK:            true,
		ID:            job.ID,
		Cluster:       job.Cluster,
		Pool:          job.Pool,
		Namespace:     job.Namespace,
		Policy:        job.Policy,
		State:         job.State,
		Cursor:        job.Cursor,
		Scanned:       job.Scanned,
		OrphansFound:  job.OrphansFound,
		OrphansGC:     job.OrphansGC,
		OrphansReport: job.OrphansReport,
		AbsentBackref: job.AbsentBackref,
		Errors:        job.Errors,
		Message:       job.Message,
	}
}

func writeAdminJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
