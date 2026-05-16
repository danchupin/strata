package adminapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/s3api"
)

// BucketPlacementJSON is the operator-console wire shape for the per-bucket
// placement policy (US-001 placement-rebalance). Weights are keyed by
// cluster id and bounded `[0, 100]`; at least one must be positive.
//
// Mode (US-001 effective-placement) is the bucket's placement-mode
// override: "weighted" (default) lets EffectivePolicy fall back to the
// cluster-weights default policy when the bucket's Placement points
// only at draining clusters; "strict" disables the fallback and makes
// PUTs return 503 DrainRefused / drain workflows refuse to fire. GET
// always returns one of {"weighted", "strict"} (empty string in storage
// is coerced to "weighted" — the backwards-compat default for legacy
// buckets).
type BucketPlacementJSON struct {
	Placement map[string]int `json:"placement"`
	Mode      string         `json:"mode,omitempty"`
}

// bucketPlacementPutRequest is the wire shape PUT
// /admin/v1/buckets/{name}/placement accepts. Mode is a pointer so the
// handler can tell "field absent" from "field set to empty" — only when
// the field is present do we route through SetBucketPlacementMode +
// stamp the admin:UpdateBucketPlacementMode audit action.
type bucketPlacementPutRequest struct {
	Placement map[string]int `json:"placement"`
	Mode      *string        `json:"mode,omitempty"`
}

// handleBucketGetPlacement serves GET /admin/v1/buckets/{bucket}/placement.
// Returns 200 + BucketPlacementJSON when configured, 404 NoSuchPlacement
// when no policy row exists, 404 NoSuchBucket when the bucket is missing.
// The response always carries `mode` (empty/NULL storage is coerced to
// "weighted" — US-001 effective-placement).
func (s *Server) handleBucketGetPlacement(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("bucket")
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "bucket name is required")
		return
	}
	if s.Meta == nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", "meta store not configured")
		return
	}
	policy, err := s.Meta.GetBucketPlacement(r.Context(), name)
	if err != nil {
		if errors.Is(err, meta.ErrBucketNotFound) {
			writeJSONError(w, http.StatusNotFound, "NoSuchBucket", "bucket not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	if policy == nil {
		writeJSONError(w, http.StatusNotFound, "NoSuchPlacement",
			"no placement policy configured on bucket")
		return
	}
	b, err := s.Meta.GetBucket(r.Context(), name)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, BucketPlacementJSON{
		Placement: policy,
		Mode:      meta.NormalizePlacementMode(b.PlacementMode),
	})
}

// handleBucketSetPlacement serves PUT /admin/v1/buckets/{bucket}/placement.
// Body: BucketPlacementJSON. Validates weights and (when configured) that
// every cluster id resolves against KnownClusters; audit row stamped as
// admin:PutBucketPlacement.
func (s *Server) handleBucketSetPlacement(w http.ResponseWriter, r *http.Request) {
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
	var req bucketPlacementPutRequest
	if jerr := json.Unmarshal(body, &req); jerr != nil {
		writeJSONError(w, http.StatusBadRequest, "MalformedRequest", "invalid JSON: "+jerr.Error())
		return
	}
	if err := meta.ValidatePlacement(req.Placement); err != nil {
		writeJSONError(w, http.StatusBadRequest, "InvalidPlacement",
			"placement weights must be in [0, 100] with sum > 0 and non-empty cluster ids")
		return
	}
	if unknown := s.unknownPlacementClusters(req.Placement); len(unknown) > 0 {
		writeJSONError(w, http.StatusBadRequest, "UnknownCluster",
			"placement references unconfigured cluster id(s): "+joinSorted(unknown))
		return
	}
	if req.Mode != nil {
		if err := meta.ValidatePlacementMode(*req.Mode); err != nil {
			writeJSONError(w, http.StatusBadRequest, "InvalidPlacementMode",
				"placement mode must be \"weighted\" or \"strict\"")
			return
		}
	}

	ctx := r.Context()
	owner := auth.FromContext(ctx).Owner
	action := "admin:PutBucketPlacement"
	if req.Mode != nil {
		// The operator explicitly opted into the mode-aware write —
		// stamp the audit so an external reviewer can spot strict-
		// flag flips on the audit log (US-001 effective-placement).
		action = "admin:UpdateBucketPlacementMode"
	}
	s3api.SetAuditOverride(ctx, action, "bucket:"+name, name, owner)

	if err := s.Meta.SetBucketPlacement(ctx, name, req.Placement); err != nil {
		if errors.Is(err, meta.ErrBucketNotFound) {
			writeJSONError(w, http.StatusNotFound, "NoSuchBucket", "bucket not found")
			return
		}
		if errors.Is(err, meta.ErrInvalidPlacement) {
			writeJSONError(w, http.StatusBadRequest, "InvalidPlacement",
				"placement weights must be in [0, 100] with sum > 0 and non-empty cluster ids")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	if req.Mode != nil {
		if err := s.Meta.SetBucketPlacementMode(ctx, name, *req.Mode); err != nil {
			if errors.Is(err, meta.ErrBucketNotFound) {
				writeJSONError(w, http.StatusNotFound, "NoSuchBucket", "bucket not found")
				return
			}
			if errors.Is(err, meta.ErrInvalidPlacementMode) {
				writeJSONError(w, http.StatusBadRequest, "InvalidPlacementMode",
					"placement mode must be \"weighted\" or \"strict\"")
				return
			}
			writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
			return
		}
	}
	// Placement change invalidates any /drain-impact preview the operator
	// may have just generated — flush synchronously so the next bulk-fix
	// roundtrip reflects the new policy without waiting drainImpactCacheTTL
	// (US-002 drain-cleanup).
	s.drainImpact().InvalidateAll()
	w.WriteHeader(http.StatusNoContent)
}

// handleBucketDeletePlacement serves DELETE /admin/v1/buckets/{bucket}/placement.
// Idempotent — returns 204 even when no policy was configured. Audit:
// admin:DeleteBucketPlacement.
func (s *Server) handleBucketDeletePlacement(w http.ResponseWriter, r *http.Request) {
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
	s3api.SetAuditOverride(ctx, "admin:DeleteBucketPlacement", "bucket:"+name, name, owner)
	if err := s.Meta.DeleteBucketPlacement(ctx, name); err != nil {
		if errors.Is(err, meta.ErrBucketNotFound) {
			writeJSONError(w, http.StatusNotFound, "NoSuchBucket", "bucket not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	// Same rationale as the PUT path — dropping the policy reverts the
	// bucket to default routing and the /drain-impact categorization
	// changes (US-002 drain-cleanup).
	s.drainImpact().InvalidateAll()
	w.WriteHeader(http.StatusNoContent)
}

// unknownPlacementClusters returns the subset of cluster ids in policy that
// do NOT appear in s.KnownClusters. Returns nil when the operator hasn't
// configured a known-cluster set (memory backend, dev rigs) so validation
// is a no-op — production wiring always passes the real cluster list.
func (s *Server) unknownPlacementClusters(policy map[string]int) []string {
	if s.KnownClusters == nil {
		return nil
	}
	var bad []string
	for id := range policy {
		if _, ok := s.KnownClusters[id]; !ok {
			bad = append(bad, id)
		}
	}
	return bad
}

func joinSorted(ids []string) string {
	sort.Strings(ids)
	return strings.Join(ids, ", ")
}
