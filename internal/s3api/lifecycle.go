package s3api

import (
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/lifecycle"
	"github.com/danchupin/strata/internal/meta"
)

func (s *Server) putBucketLifecycle(w http.ResponseWriter, r *http.Request, bucket string) {
	b, err := s.Meta.GetBucket(r.Context(), bucket)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, r, ErrMalformedXML)
		return
	}
	cfg, parseErr := lifecycle.Parse(body)
	if parseErr != nil {
		writeError(w, r, ErrMalformedXML)
		return
	}
	if vErr := cfg.Validate(); vErr != nil {
		writeError(w, r, APIError{Code: "InvalidArgument", Message: vErr.Error(), Status: http.StatusBadRequest})
		return
	}
	if err := s.Meta.SetBucketLifecycle(r.Context(), b.ID, body); err != nil {
		mapMetaErr(w, r, err)
		return
	}
	// US-014: when the data backend supports bidirectional lifecycle mapping
	// (s3 backend), translate the user-supplied rules into a backend bucket
	// lifecycle so native transitions/expirations run on the backend side.
	// Strata-stored config remains the source of truth; backend config is
	// derived state. Translation failures log WARN and do NOT fail the
	// user request — the worker keeps owning everything.
	if lb, ok := s.Data.(data.LifecycleBackend); ok {
		rules := translateLifecycleRules(cfg)
		skipped, err := lb.PutBackendLifecycle(r.Context(), b.ID.String()+"/", rules)
		if err != nil {
			slog.Warn("s3 lifecycle backend translation: push failed",
				"bucket", bucket, "err", err)
		}
		for _, id := range skipped {
			slog.Warn("s3 lifecycle backend translation: rule kept on strata worker (non-native transition)",
				"bucket", bucket, "rule", id)
		}
	}
	w.WriteHeader(http.StatusOK)
}

// translateLifecycleRules flattens a Strata lifecycle.Configuration into the
// data.LifecycleRule shape the backend translator consumes. Drops disabled
// rules and rules with no translatable action (NoncurrentVersion* or
// Tag-only filters stay strata-side because the backend interface doesn't
// expose them).
func translateLifecycleRules(cfg *lifecycle.Configuration) []data.LifecycleRule {
	out := make([]data.LifecycleRule, 0, len(cfg.Rules))
	for _, r := range cfg.Rules {
		if !r.IsEnabled() {
			continue
		}
		entry := data.LifecycleRule{ID: r.ID, Prefix: r.PrefixMatch()}
		if r.Transition != nil && r.Transition.Days > 0 && r.Transition.StorageClass != "" {
			entry.TransitionDays = r.Transition.Days
			entry.TransitionStorageClass = r.Transition.StorageClass
		}
		if r.Expiration != nil && r.Expiration.Days > 0 {
			entry.ExpirationDays = r.Expiration.Days
		}
		if r.AbortIncompleteMultipartUpload != nil && r.AbortIncompleteMultipartUpload.DaysAfterInitiation > 0 {
			entry.AbortIncompleteUploadDays = r.AbortIncompleteMultipartUpload.DaysAfterInitiation
		}
		// Skip rules with no translatable action — noncurrent-only rules
		// stay on the strata worker without bothering the backend.
		if entry.TransitionDays == 0 && entry.ExpirationDays == 0 && entry.AbortIncompleteUploadDays == 0 {
			continue
		}
		out = append(out, entry)
	}
	return out
}

func (s *Server) getBucketLifecycle(w http.ResponseWriter, r *http.Request, bucket string) {
	b, err := s.Meta.GetBucket(r.Context(), bucket)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	rules, err := s.Meta.GetBucketLifecycle(r.Context(), b.ID)
	if err != nil {
		if errors.Is(err, meta.ErrNoSuchLifecycle) {
			writeError(w, r, ErrNoSuchLifecycleConfiguration)
			return
		}
		mapMetaErr(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(rules)
}

func (s *Server) deleteBucketLifecycle(w http.ResponseWriter, r *http.Request, bucket string) {
	b, err := s.Meta.GetBucket(r.Context(), bucket)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	if err := s.Meta.DeleteBucketLifecycle(r.Context(), b.ID); err != nil {
		mapMetaErr(w, r, err)
		return
	}
	// US-014: clear the backend bucket lifecycle too so derived state
	// doesn't outlive the source of truth. Errors are non-fatal — the user
	// already saw success at the meta layer; backend cleanup is best-effort.
	if lb, ok := s.Data.(data.LifecycleBackend); ok {
		if err := lb.DeleteBackendLifecycle(r.Context(), b.ID.String()+"/"); err != nil {
			slog.Warn("s3 lifecycle backend translation: delete failed",
				"bucket", bucket, "err", err)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}
