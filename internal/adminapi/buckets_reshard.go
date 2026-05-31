package adminapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/s3api"
)

// BucketReshardJSON is the operator-console wire shape for the per-bucket
// online reshard (US-006). It mirrors the s3api admin endpoint
// (adminBucketReshardResponse) but rides the cookie-authenticated /admin/v1
// surface the web console talks to, and adds `supported` so the UI can render
// the disabled-with-tooltip state on a range-scan backend.
//
// State is one of:
//   - "idle"    — no job in flight; the bucket sits at ShardCount.
//   - "queued"  — StartReshard created a job, the worker has not advanced the
//     LastKey watermark yet.
//   - "running" — the worker has advanced LastKey at least once.
//
// Supported reflects whether the active meta backend physically moves rows on
// a reshard. Only Cassandra implements meta.ReshardMigrator; TiKV and memory
// are range-scan / flat-map backends where a reshard is an immediate-complete
// no-op (the partition layout carries no per-key shard). The UI disables the
// Reshard action when supported=false and explains why — API stays a no-op
// success, UI stays disabled, both consistent.
type BucketReshardJSON struct {
	OK         bool   `json:"ok"`
	Bucket     string `json:"bucket"`
	Supported  bool   `json:"supported"`
	State      string `json:"state"`
	Source     int    `json:"source,omitempty"`
	Target     int    `json:"target,omitempty"`
	ShardCount int    `json:"shard_count"`
	LastKey    string `json:"last_key,omitempty"`
	StartedAt  int64  `json:"started_at,omitempty"`
	UpdatedAt  int64  `json:"updated_at,omitempty"`
}

// bucketReshardPostRequest is the JSON body accepted by
// POST /admin/v1/buckets/{bucket}/reshard. Target must be a positive power of
// two strictly greater than the current shard count.
type bucketReshardPostRequest struct {
	Target int `json:"target"`
}

// unixOrZero renders a time as a Unix-seconds wire value, coercing the zero
// time to 0 so an unset CreatedAt/UpdatedAt serialises as an absent field
// (json:",omitempty") rather than a 1970 timestamp.
func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// metaBackendSupportsReshard reports whether the active meta backend physically
// relocates rows on a reshard. Kept in lockstep with the optional
// meta.ReshardMigrator interface — only Cassandra implements it.
func (s *Server) metaBackendSupportsReshard() bool {
	return s.MetaBackend == "cassandra"
}

// handleBucketGetReshard serves GET /admin/v1/buckets/{bucket}/reshard. It
// reports the active reshard job (queued/running) or the steady-state shard
// count (idle) so the console can poll a reshard to completion the same way
// the DrainProgressBar polls a drain.
func (s *Server) handleBucketGetReshard(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("bucket")
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "bucket name is required")
		return
	}
	if s.Meta == nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", "meta store not configured")
		return
	}
	b, err := s.Meta.GetBucket(r.Context(), name)
	if err != nil {
		if errors.Is(err, meta.ErrBucketNotFound) {
			writeJSONError(w, http.StatusNotFound, "NoSuchBucket", "bucket not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	supported := s.metaBackendSupportsReshard()
	job, err := s.Meta.GetReshardJob(r.Context(), b.ID)
	if errors.Is(err, meta.ErrReshardNotFound) {
		writeJSON(w, http.StatusOK, BucketReshardJSON{
			OK:         true,
			Bucket:     name,
			Supported:  supported,
			State:      "idle",
			ShardCount: b.ShardCount,
		})
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	state := "queued"
	if job.LastKey != "" {
		state = "running"
	}
	writeJSON(w, http.StatusOK, BucketReshardJSON{
		OK:         true,
		Bucket:     name,
		Supported:  supported,
		State:      state,
		Source:     job.Source,
		Target:     job.Target,
		ShardCount: b.ShardCount,
		LastKey:    job.LastKey,
		StartedAt:  unixOrZero(job.CreatedAt),
		UpdatedAt:  unixOrZero(job.UpdatedAt),
	})
}

// handleBucketReshard serves POST /admin/v1/buckets/{bucket}/reshard. It queues
// an online shard-resize and returns 202 immediately — the leader-elected
// `reshard` worker drives the physical row migration out of band (a large
// bucket's 64→128 move must never block the request goroutine). Audit row is
// stamped admin:BucketReshard.
func (s *Server) handleBucketReshard(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	name := r.PathValue("bucket")
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "bucket name is required")
		return
	}
	if s.Meta == nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", "meta store not configured")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<10))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "failed to read body")
		return
	}
	var req bucketReshardPostRequest
	if jerr := json.Unmarshal(body, &req); jerr != nil {
		writeJSONError(w, http.StatusBadRequest, "MalformedRequest", "invalid JSON: "+jerr.Error())
		return
	}
	if !meta.IsValidShardCount(req.Target) {
		writeJSONError(w, http.StatusBadRequest, "InvalidArgument",
			"target must be a positive power of two")
		return
	}
	b, err := s.Meta.GetBucket(r.Context(), name)
	if err != nil {
		if errors.Is(err, meta.ErrBucketNotFound) {
			writeJSONError(w, http.StatusNotFound, "NoSuchBucket", "bucket not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}

	ctx := r.Context()
	owner := auth.FromContext(ctx).Owner
	s3api.SetAuditOverride(ctx, "admin:BucketReshard", "bucket:"+name, name, owner)

	job, err := s.Meta.StartReshard(ctx, b.ID, req.Target)
	if err != nil {
		switch {
		case errors.Is(err, meta.ErrReshardInProgress):
			writeJSONError(w, http.StatusConflict, "OperationAborted", err.Error())
		case errors.Is(err, meta.ErrReshardInvalidTarget):
			writeJSONError(w, http.StatusBadRequest, "InvalidArgument", err.Error())
		case errors.Is(err, meta.ErrBucketNotFound):
			writeJSONError(w, http.StatusNotFound, "NoSuchBucket", "bucket not found")
		default:
			writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		}
		return
	}
	writeJSON(w, http.StatusAccepted, BucketReshardJSON{
		OK:         true,
		Bucket:     name,
		Supported:  s.metaBackendSupportsReshard(),
		State:      "queued",
		Source:     job.Source,
		Target:     job.Target,
		ShardCount: b.ShardCount,
	})
}
