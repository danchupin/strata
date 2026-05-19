package adminapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/data/placement"
	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/s3api"
)

// BucketECPolicyJSON is the operator-console wire shape for the per-bucket
// erasure-code policy (US-007 EC-aware manifest schema). K is the number
// of data shards; M is the number of parity shards.
type BucketECPolicyJSON struct {
	K int `json:"k"`
	M int `json:"m"`
}

// inconsistentECPolicyError surfaces the cluster id whose underlying EC
// profile disagrees with the requested (k, m). Mapped to HTTP 409 by the
// admin handler so operators see the exact offending cluster.
type inconsistentECPolicyError struct {
	Cluster   string
	WantK     int
	WantM     int
	HaveEC    bool
	HaveK     int
	HaveM     int
}

func (e *inconsistentECPolicyError) Error() string {
	if !e.HaveEC {
		return fmt.Sprintf("cluster %q is replicated, cannot satisfy k=%d m=%d", e.Cluster, e.WantK, e.WantM)
	}
	return fmt.Sprintf("cluster %q has k=%d m=%d, requested k=%d m=%d",
		e.Cluster, e.HaveK, e.HaveM, e.WantK, e.WantM)
}

// handleBucketGetECPolicy serves GET /admin/v1/buckets/{bucket}/ec-policy.
// Returns 200 + BucketECPolicyJSON when configured, 404 NoSuchECPolicy
// when no policy is set, 404 NoSuchBucket when the bucket is missing.
func (s *Server) handleBucketGetECPolicy(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("bucket")
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "bucket name is required")
		return
	}
	if s.Meta == nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", "meta store not configured")
		return
	}
	p, err := s.Meta.GetBucketECPolicy(r.Context(), name)
	if err != nil {
		if errors.Is(err, meta.ErrBucketNotFound) {
			writeJSONError(w, http.StatusNotFound, "NoSuchBucket", "bucket not found")
			return
		}
		if errors.Is(err, meta.ErrNoSuchECPolicy) {
			writeJSONError(w, http.StatusNotFound, "NoSuchECPolicy",
				"no EC policy configured on bucket")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, BucketECPolicyJSON{K: p.K, M: p.M})
}

// handleBucketSetECPolicy serves PUT /admin/v1/buckets/{bucket}/ec-policy.
// Body: BucketECPolicyJSON. Validates structurally then probes every
// cluster in the bucket's effective placement via the data backend's
// ClusterECCapability interface. Mismatch → 409 InconsistentECPolicy.
// Audit verb: admin:UpdateBucketECPolicy.
func (s *Server) handleBucketSetECPolicy(w http.ResponseWriter, r *http.Request) {
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
	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<10))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "failed to read body")
		return
	}
	var req BucketECPolicyJSON
	if jerr := json.Unmarshal(body, &req); jerr != nil {
		writeJSONError(w, http.StatusBadRequest, "MalformedRequest", "invalid JSON: "+jerr.Error())
		return
	}
	policy := meta.ECPolicy{K: req.K, M: req.M}
	if err := meta.ValidateECPolicy(policy); err != nil {
		writeJSONError(w, http.StatusBadRequest, "InvalidECPolicy",
			"EC policy k and m must both be > 0")
		return
	}

	ctx := r.Context()
	targets, err := s.ecValidationTargets(ctx, name)
	if err != nil {
		if errors.Is(err, meta.ErrBucketNotFound) {
			writeJSONError(w, http.StatusNotFound, "NoSuchBucket", "bucket not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	if len(targets) == 0 {
		writeJSONError(w, http.StatusConflict, "NoPlacement",
			"bucket has no placement targets — set placement before setting EC policy")
		return
	}
	probe, ok := s.Data.(data.ClusterECCapability)
	if !ok {
		// Backend cannot probe; refuse rather than silently accept.
		writeJSONError(w, http.StatusConflict, "InconsistentECPolicy",
			"data backend does not expose EC capability probe")
		return
	}
	for _, cluster := range targets {
		ecOK, k, m, perr := probe.ClusterECCapability(ctx, cluster)
		if perr != nil {
			writeJSONError(w, http.StatusInternalServerError, "Internal",
				fmt.Sprintf("probe cluster %q: %v", cluster, perr))
			return
		}
		if !ecOK || k != req.K || m != req.M {
			mismatch := &inconsistentECPolicyError{
				Cluster: cluster,
				WantK:   req.K, WantM: req.M,
				HaveEC: ecOK, HaveK: k, HaveM: m,
			}
			writeJSONError(w, http.StatusConflict, "InconsistentECPolicy", mismatch.Error())
			return
		}
	}

	owner := auth.FromContext(ctx).Owner
	s3api.SetAuditOverride(ctx, "admin:UpdateBucketECPolicy", "bucket:"+name, name, owner)
	if err := s.Meta.SetBucketECPolicy(ctx, name, policy); err != nil {
		if errors.Is(err, meta.ErrBucketNotFound) {
			writeJSONError(w, http.StatusNotFound, "NoSuchBucket", "bucket not found")
			return
		}
		if errors.Is(err, meta.ErrInvalidECPolicy) {
			writeJSONError(w, http.StatusBadRequest, "InvalidECPolicy",
				"EC policy k and m must both be > 0")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleBucketDeleteECPolicy serves DELETE
// /admin/v1/buckets/{bucket}/ec-policy. Idempotent — returns 204 even
// when no policy was configured. Audit: admin:DeleteBucketECPolicy.
func (s *Server) handleBucketDeleteECPolicy(w http.ResponseWriter, r *http.Request) {
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
	s3api.SetAuditOverride(ctx, "admin:DeleteBucketECPolicy", "bucket:"+name, name, owner)
	if err := s.Meta.DeleteBucketECPolicy(ctx, name); err != nil {
		if errors.Is(err, meta.ErrBucketNotFound) {
			writeJSONError(w, http.StatusNotFound, "NoSuchBucket", "bucket not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ecValidationTargets resolves the set of cluster ids the requested EC
// policy must match against. The set is the union of the bucket's
// explicit placement entries (with positive weight) and, when no
// explicit policy exists, the synthesised cluster-weights default
// policy from DrainCache. Returns an empty slice (not nil) when the
// bucket exists but has neither.
func (s *Server) ecValidationTargets(ctx context.Context, name string) ([]string, error) {
	policy, err := s.Meta.GetBucketPlacement(ctx, name)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0)
	if len(policy) > 0 {
		for cluster, weight := range policy {
			if weight > 0 {
				out = append(out, cluster)
			}
		}
		return out, nil
	}
	if s.DrainCache != nil {
		def := placement.DefaultPolicy(s.DrainCache.States(ctx))
		for cluster, weight := range def {
			if weight > 0 {
				out = append(out, cluster)
			}
		}
	}
	return out, nil
}
