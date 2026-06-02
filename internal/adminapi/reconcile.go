package adminapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/s3api"
)

// ReconcileJSON is the operator-console wire shape for a reconcile job (US-006).
// It mirrors the s3api admin endpoint (adminReconcileResponse) but rides the
// cookie-authenticated /admin/v1 surface the web console talks to, and renders
// the timestamps as Unix seconds so the UI can show an elapsed clock the same
// way DrainProgressBar / the reshard panel do.
//
// State is one of "queued" | "running" | "done" | "error" (meta.ReconcileState*).
// A job carries BOTH the orphan-pass counters (Scanned/Orphans*/AbsentBackref)
// and the dangling-pass counters (ManifestsScanned/Healthy/Dangling*); which set
// is meaningful is keyed off Bucket (empty == orphan pass, non-empty == dangling
// pass) — the console renders the relevant block.
type ReconcileJSON struct {
	OK            bool   `json:"ok"`
	ID            string `json:"id"`
	Cluster       string `json:"cluster,omitempty"`
	Pool          string `json:"pool,omitempty"`
	Namespace     string `json:"namespace,omitempty"`
	Bucket        string `json:"bucket,omitempty"`
	Policy        string `json:"policy"`
	State         string `json:"state"`
	Cursor        string `json:"cursor,omitempty"`
	Scanned       int64  `json:"scanned"`
	OrphansFound   int64 `json:"orphans_found"`
	OrphansGC      int64 `json:"orphans_gc"`
	OrphansReport  int64 `json:"orphans_report"`
	OrphansRestore int64 `json:"orphans_restore"`
	AbsentBackref  int64 `json:"absent_backref"`
	// Dangling-pass (US-003) counters.
	ManifestsScanned   int64  `json:"manifests_scanned"`
	Healthy            int64  `json:"healthy"`
	DanglingFound      int64  `json:"dangling_found"`
	DanglingQuarantine int64  `json:"dangling_quarantine"`
	DanglingReport     int64  `json:"dangling_report"`
	DanglingDelete     int64  `json:"dangling_delete"`
	Errors             int64  `json:"errors"`
	Message            string `json:"message,omitempty"`
	StartedAt          int64  `json:"started_at,omitempty"`
	UpdatedAt          int64  `json:"updated_at,omitempty"`
}

// reconcilePostRequest is the JSON body accepted by POST /admin/v1/reconcile.
// Two passes, discriminated by Bucket:
//   - ORPHAN (data->meta): Bucket empty. Cluster + Pool required; Namespace
//     optional. Policy is report (default), gc, or restore (US-002b — rebuild
//     the manifest row from the back-reference).
//   - DANGLING (meta->data): Bucket set (the bucket NAME, resolved to its UUID
//     here). Cluster/Pool ignored. Policy is report (default), quarantine, or
//     delete (US-003b — GC the version's chunks + remove the row).
type reconcilePostRequest struct {
	Cluster   string `json:"cluster"`
	Pool      string `json:"pool"`
	Namespace string `json:"namespace"`
	Bucket    string `json:"bucket"`
	Policy    string `json:"policy"`
}

func reconcileJSON(job *meta.ReconcileJob) ReconcileJSON {
	return ReconcileJSON{
		OK:                 true,
		ID:                 job.ID,
		Cluster:            job.Cluster,
		Pool:               job.Pool,
		Namespace:          job.Namespace,
		Bucket:             job.Bucket,
		Policy:             job.Policy,
		State:              job.State,
		Cursor:             job.Cursor,
		Scanned:            job.Scanned,
		OrphansFound:       job.OrphansFound,
		OrphansGC:          job.OrphansGC,
		OrphansReport:      job.OrphansReport,
		OrphansRestore:     job.OrphansRestore,
		AbsentBackref:      job.AbsentBackref,
		ManifestsScanned:   job.ManifestsScanned,
		Healthy:            job.Healthy,
		DanglingFound:      job.DanglingFound,
		DanglingQuarantine: job.DanglingQuarantine,
		DanglingReport:     job.DanglingReport,
		DanglingDelete:     job.DanglingDelete,
		Errors:             job.Errors,
		Message:            job.Message,
		StartedAt:          unixOrZero(job.CreatedAt),
		UpdatedAt:          unixOrZero(job.UpdatedAt),
	}
}

// handleReconcileStart serves POST /admin/v1/reconcile. It queues a reconcile
// pass and returns 202 immediately — the leader-elected `reconcile` worker
// drains the job out of band (a live-cluster scan must never block the request
// goroutine). The console polls GET /admin/v1/reconcile/{id} to watch it
// converge, mirroring the reshard-progress UX. Audit row admin:Reconcile.
func (s *Server) handleReconcileStart(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	if s.Meta == nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", "meta store not configured")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<10))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "failed to read body")
		return
	}
	var req reconcilePostRequest
	if len(body) > 0 {
		if jerr := json.Unmarshal(body, &req); jerr != nil {
			writeJSONError(w, http.StatusBadRequest, "MalformedRequest", "invalid JSON: "+jerr.Error())
			return
		}
	}
	policy := req.Policy
	if policy == "" {
		policy = meta.ReconcilePolicyReport
	}

	var (
		bucketID      string
		auditResource string
	)
	if req.Bucket != "" {
		// Dangling pass: resolve the bucket name to its UUID for the worker.
		b, gerr := s.Meta.GetBucket(r.Context(), req.Bucket)
		if gerr != nil {
			if errors.Is(gerr, meta.ErrBucketNotFound) {
				writeJSONError(w, http.StatusNotFound, "NoSuchBucket", "bucket not found")
				return
			}
			writeJSONError(w, http.StatusInternalServerError, "Internal", gerr.Error())
			return
		}
		bucketID = b.ID.String()
		auditResource = "bucket:" + req.Bucket
	} else {
		if req.Cluster == "" || req.Pool == "" {
			writeJSONError(w, http.StatusBadRequest, "InvalidArgument",
				"cluster and pool are required (or pass bucket for a dangling-manifest pass)")
			return
		}
		auditResource = "cluster:" + req.Cluster + "/" + req.Pool
	}

	ctx := r.Context()
	owner := auth.FromContext(ctx).Owner
	s3api.SetAuditOverride(ctx, "admin:Reconcile", auditResource, "", owner)

	job, err := s.Meta.StartReconcile(ctx, req.Cluster, req.Pool, req.Namespace, bucketID, policy)
	if err != nil {
		if errors.Is(err, meta.ErrReconcileInvalidPolicy) {
			writeJSONError(w, http.StatusBadRequest, "InvalidArgument", err.Error())
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, reconcileJSON(job))
}

// handleReconcileStatus serves GET /admin/v1/reconcile/{id}. It reads a job by
// id so the console can poll a pass to completion and render the post-run
// orphan / dangling summary.
func (s *Server) handleReconcileStatus(w http.ResponseWriter, r *http.Request) {
	if s.Meta == nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", "meta store not configured")
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "job id is required")
		return
	}
	job, err := s.Meta.GetReconcileJob(r.Context(), id)
	if errors.Is(err, meta.ErrReconcileNotFound) {
		writeJSONError(w, http.StatusNotFound, "NoSuchReconcileJob", "no reconcile job with that id")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, reconcileJSON(job))
}
