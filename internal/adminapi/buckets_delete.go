package adminapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/leader"
	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/s3api"
)

// forceEmptyLockTTL is the lease duration we hold against worker_locks
// while a per-bucket force-empty job is running. Longer than the typical
// drain so PD-style backend hiccups do not lose the lease mid-flight; the
// goroutine renews it implicitly via UpdateAdminJob.
const forceEmptyLockTTL = 10 * time.Minute

// forceEmptyPollListLimit is the per-page Object listing size for the
// drain loop. AWS-S3 caps ListObjects at 1000; bigger pages would just
// add to the round-trip latency without helping.
const forceEmptyPollListLimit = 1000

// forceEmptyMaxConsecutiveFailures aborts the goroutine after this many
// back-to-back DeleteObject errors. Otherwise a permanently-broken object
// row would spin the goroutine forever without forward progress.
const forceEmptyMaxConsecutiveFailures = 5

// forceEmptyLockName returns the worker_locks key reserved for the
// per-bucket force-empty job.
func forceEmptyLockName(bucket string) string {
	return "bucket-force-empty:" + bucket
}

// handleBucketDelete serves DELETE /admin/v1/buckets/{bucket}. Returns
// 204 on success, 404 NoSuchBucket when missing, 409 BucketNotEmpty when
// objects remain. Audit row: action=admin:DeleteBucket, resource=bucket:<name>.
func (s *Server) handleBucketDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("bucket")
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "bucket name is required")
		return
	}
	if s.Meta == nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", "meta store not configured")
		return
	}
	ctx := r.Context()
	owner := auth.FromContext(ctx).Owner
	s3api.SetAuditOverride(ctx, "admin:DeleteBucket", "bucket:"+name, name, owner)

	err := s.Meta.DeleteBucket(ctx, name)
	switch {
	case err == nil:
		// Bucket vanishing flips its chunks off every /drain-impact
		// preview — drop the cache synchronously so the next GET
		// reflects reality (US-002 drain-cleanup).
		s.drainImpact().InvalidateAll()
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, meta.ErrBucketNotFound):
		writeJSONError(w, http.StatusNotFound, "NoSuchBucket", "bucket not found")
	case errors.Is(err, meta.ErrBucketNotEmpty):
		writeJSONError(w, http.StatusConflict, "BucketNotEmpty",
			"bucket has objects; use POST /admin/v1/buckets/<bucket>/force-empty to drain it first")
	default:
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
	}
}

// handleBucketForceEmpty serves POST /admin/v1/buckets/{bucket}/force-empty.
// Acquires the bucket-force-empty:<bucket> lease in worker_locks, persists
// a fresh AdminJob row, and kicks a goroutine that paginates ListObjects +
// DeleteObject until the bucket is empty. Returns 202 + ForceEmptyJobResponse
// on success, 404 NoSuchBucket when missing, 409 Conflict when another job
// already holds the lease. Audit row: admin:ForceEmpty, resource=bucket:<name>.
func (s *Server) handleBucketForceEmpty(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("bucket")
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "bucket name is required")
		return
	}
	if s.Meta == nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", "meta store not configured")
		return
	}
	if s.Locker == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "LockerUnavailable",
			"meta backend exposes no leader-election locker; force-empty unavailable")
		return
	}
	ctx := r.Context()
	owner := auth.FromContext(ctx).Owner
	s3api.SetAuditOverride(ctx, "admin:ForceEmpty", "bucket:"+name, name, owner)

	if _, err := s.Meta.GetBucket(ctx, name); err != nil {
		if errors.Is(err, meta.ErrBucketNotFound) {
			writeJSONError(w, http.StatusNotFound, "NoSuchBucket", "bucket not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}

	lockName := forceEmptyLockName(name)
	holder := leader.DefaultHolder()
	acquired, err := s.Locker.Acquire(ctx, lockName, holder, forceEmptyLockTTL)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal",
			fmt.Sprintf("acquire lock: %v", err))
		return
	}
	if !acquired {
		writeJSONError(w, http.StatusConflict, "ForceEmptyInProgress",
			"another force-empty job is already running for this bucket")
		return
	}

	now := time.Now().UTC()
	job := &meta.AdminJob{
		ID:        uuid.NewString(),
		Kind:      meta.AdminJobKindForceEmpty,
		Bucket:    name,
		State:     meta.AdminJobStatePending,
		StartedAt: now,
		UpdatedAt: now,
	}
	if err := s.Meta.CreateAdminJob(ctx, job); err != nil {
		// Always release the lease before returning — otherwise the
		// holder gets stranded and the bucket stays locked until TTL.
		_ = s.Locker.Release(context.Background(), lockName, holder)
		writeJSONError(w, http.StatusInternalServerError, "Internal",
			fmt.Sprintf("persist admin job: %v", err))
		return
	}

	resp := jobToResponse(job)
	// Copy the *AdminJob before handing it to the goroutine so the
	// goroutine's mutations don't race the handler's response build.
	jobCopy := *job
	s.startForceEmptyJob(&jobCopy, lockName, holder)

	writeJSON(w, http.StatusAccepted, resp)
}

// handleBucketForceEmptyStatus serves GET /admin/v1/buckets/{bucket}/
// force-empty/{jobID}. Returns 200 + ForceEmptyJobResponse (any state),
// 404 when the job ID is unknown OR points at a different bucket.
func (s *Server) handleBucketForceEmptyStatus(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("bucket")
	jobID := r.PathValue("jobID")
	if name == "" || jobID == "" {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "bucket and jobID are required")
		return
	}
	if s.Meta == nil {
		writeJSONError(w, http.StatusNotFound, "JobNotFound", "job not found")
		return
	}
	job, err := s.Meta.GetAdminJob(r.Context(), jobID)
	if err != nil {
		if errors.Is(err, meta.ErrAdminJobNotFound) {
			writeJSONError(w, http.StatusNotFound, "JobNotFound", "job not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	if job.Bucket != name {
		writeJSONError(w, http.StatusNotFound, "JobNotFound", "job not found for this bucket")
		return
	}
	writeJSON(w, http.StatusOK, jobToResponse(job))
}

// jobToResponse projects a meta.AdminJob row onto the wire shape.
func jobToResponse(j *meta.AdminJob) ForceEmptyJobResponse {
	out := ForceEmptyJobResponse{
		JobID:     j.ID,
		Bucket:    j.Bucket,
		State:     j.State,
		Deleted:   j.Deleted,
		Message:   j.Message,
		StartedAt: j.StartedAt.Unix(),
		UpdatedAt: j.UpdatedAt.Unix(),
	}
	if !j.FinishedAt.IsZero() {
		out.FinishedAt = j.FinishedAt.Unix()
	}
	return out
}

// startForceEmptyJob kicks the drain goroutine and registers it on the
// Server's WaitGroup so a graceful shutdown can wait for it. The goroutine
// is responsible for releasing the worker_locks lease + flipping the
// AdminJob row to a terminal state.
func (s *Server) startForceEmptyJob(job *meta.AdminJob, lockName, holder string) {
	s.jobsMu.Lock()
	s.jobsWG.Add(1)
	s.jobsMu.Unlock()
	go func() {
		defer s.jobsWG.Done()
		s.runForceEmptyJob(context.Background(), job, lockName, holder)
	}()
}

// runForceEmptyJob is the body of the force-empty goroutine. Walks
// ListObjectVersions in pages and issues one DeleteObject per row until
// the bucket is empty (or a hard error short-circuits the loop). Updates
// the AdminJob row with running tally + state changes, and always
// releases the worker_locks lease on exit so a follow-up retry can run.
func (s *Server) runForceEmptyJob(ctx context.Context, job *meta.AdminJob, lockName, holder string) {
	defer func() {
		if rerr := s.Locker.Release(ctx, lockName, holder); rerr != nil {
			s.Logger.Printf("adminapi: force-empty release lease %q: %v", lockName, rerr)
		}
	}()

	job.State = meta.AdminJobStateRunning
	job.UpdatedAt = time.Now().UTC()
	if err := s.Meta.UpdateAdminJob(ctx, job); err != nil {
		s.Logger.Printf("adminapi: force-empty %s mark running: %v", job.ID, err)
		return
	}

	bucket, err := s.Meta.GetBucket(ctx, job.Bucket)
	if err != nil {
		s.finishForceEmpty(ctx, job, meta.AdminJobStateError, fmt.Sprintf("get bucket: %v", err))
		return
	}

	failures := 0
	for {
		drained, drainErr := s.forceEmptyDrainPage(ctx, bucket.ID, job)
		if drainErr != nil {
			failures++
			s.Logger.Printf("adminapi: force-empty %s drain page: %v", job.ID, drainErr)
			if failures >= forceEmptyMaxConsecutiveFailures {
				s.finishForceEmpty(ctx, job, meta.AdminJobStateError,
					fmt.Sprintf("aborted after %d consecutive failures: %v", failures, drainErr))
				return
			}
			time.Sleep(time.Second)
			continue
		}
		failures = 0
		if drained == 0 {
			break
		}
		// Flush the running tally periodically so the polling client can
		// observe forward progress.
		job.UpdatedAt = time.Now().UTC()
		if err := s.Meta.UpdateAdminJob(ctx, job); err != nil {
			s.Logger.Printf("adminapi: force-empty %s persist progress: %v", job.ID, err)
		}
	}
	s.finishForceEmpty(ctx, job, meta.AdminJobStateDone, "")
}

// forceEmptyDrainPage walks one ListObjectVersions page and DeleteObject's
// every row. Returns the number of rows successfully deleted on this page;
// 0 means the bucket is fully drained.
func (s *Server) forceEmptyDrainPage(ctx context.Context, bucketID uuid.UUID, job *meta.AdminJob) (int, error) {
	res, err := s.Meta.ListObjectVersions(ctx, bucketID, meta.ListOptions{
		Limit: forceEmptyPollListLimit,
	})
	if err != nil {
		return 0, fmt.Errorf("list versions: %w", err)
	}
	if len(res.Versions) == 0 {
		// ListObjectVersions returned nothing — fall back to ListObjects
		// in case the backend's versioning mode masks the row layout
		// (memory store goes through both paths).
		objs, err := s.Meta.ListObjects(ctx, bucketID, meta.ListOptions{
			Limit: forceEmptyPollListLimit,
		})
		if err != nil {
			return 0, fmt.Errorf("list objects: %w", err)
		}
		if len(objs.Objects) == 0 {
			return 0, nil
		}
		deleted := 0
		for _, o := range objs.Objects {
			if _, err := s.Meta.DeleteObject(ctx, bucketID, o.Key, "", false); err != nil {
				return deleted, fmt.Errorf("delete %s: %w", o.Key, err)
			}
			deleted++
			job.Deleted++
		}
		return deleted, nil
	}
	deleted := 0
	for _, v := range res.Versions {
		if _, err := s.Meta.DeleteObject(ctx, bucketID, v.Key, v.VersionID, true); err != nil {
			return deleted, fmt.Errorf("delete %s@%s: %w", v.Key, v.VersionID, err)
		}
		deleted++
		job.Deleted++
	}
	return deleted, nil
}

// finishForceEmpty stamps the terminal state + message + finished_at and
// flushes the row. Errors are best-effort — the caller is exiting anyway.
func (s *Server) finishForceEmpty(ctx context.Context, job *meta.AdminJob, state, message string) {
	job.State = state
	job.Message = message
	now := time.Now().UTC()
	job.UpdatedAt = now
	job.FinishedAt = now
	if err := s.Meta.UpdateAdminJob(ctx, job); err != nil {
		s.Logger.Printf("adminapi: force-empty %s mark %s: %v", job.ID, state, err)
	}
}
