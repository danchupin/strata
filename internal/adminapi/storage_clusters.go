package adminapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/s3api"
)

// clusterIDPattern enforces the cluster-registry id charset (1..64 chars,
// lowercase alphanumerics + dash). Mirrors the AC for POST /admin/v1/
// storage/clusters body validation; the meta-store layer is opaque to id
// shape so this is the operator-facing guardrail.
var clusterIDPattern = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)

// clusterRegistryBackends is the accept-set for the backend discriminator
// on POST /admin/v1/storage/clusters. The registry layer is opaque to spec
// internals but the discriminator narrows which data-backend will consume
// the spec at watcher time.
var clusterRegistryBackends = map[string]struct{}{
	"rados": {},
	"s3":    {},
}

// ClusterEntryResponse is the wire shape for one cluster registry row on
// GET /admin/v1/storage/clusters and POST. Spec is the raw JSON document
// the operator submitted — the registry layer does not interpret it.
type ClusterEntryResponse struct {
	ID        string          `json:"id"`
	Backend   string          `json:"backend"`
	Spec      json.RawMessage `json:"spec"`
	CreatedAt int64           `json:"created_at"`
	UpdatedAt int64           `json:"updated_at"`
	Version   int64           `json:"version"`
}

// ClustersListResponse is the JSON shape returned by GET /admin/v1/storage/
// clusters. Entries are sorted by ID ascending — same order
// meta.Store.ListClusters guarantees.
type ClustersListResponse struct {
	Clusters []ClusterEntryResponse `json:"clusters"`
}

// CreateClusterRequest is the JSON body accepted by POST /admin/v1/storage/
// clusters. Spec is forwarded to meta.PutCluster verbatim — the consuming
// backend (rados / s3) decodes it on next watcher tick.
type CreateClusterRequest struct {
	ID      string          `json:"id"`
	Backend string          `json:"backend"`
	Spec    json.RawMessage `json:"spec"`
}

// ClusterReferencedResponse is the 409 body returned by DELETE when the
// target cluster is referenced by one or more in-process storage classes.
type ClusterReferencedResponse struct {
	Code         string   `json:"code"`
	Message      string   `json:"message"`
	ReferencedBy []string `json:"referenced_by"`
}

// handleListClusters serves GET /admin/v1/storage/clusters. Returns 200 +
// ClustersListResponse sorted by ID asc.
func (s *Server) handleListClusters(w http.ResponseWriter, r *http.Request) {
	if s.Meta == nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", "meta store not configured")
		return
	}
	ctx := r.Context()
	owner := auth.FromContext(ctx).Owner
	s3api.SetAuditOverride(ctx, "admin:ListClusters", "cluster:-", "-", owner)

	entries, err := s.Meta.ListClusters(ctx)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	out := make([]ClusterEntryResponse, 0, len(entries))
	for _, e := range entries {
		out = append(out, clusterToResponse(e))
	}
	writeJSON(w, http.StatusOK, ClustersListResponse{Clusters: out})
}

// handleCreateCluster serves POST /admin/v1/storage/clusters. Validates the
// body, hands off to meta.PutCluster (insert path), and returns the freshly
// written row. Conflict on existing id → 409 ClusterAlreadyExists.
func (s *Server) handleCreateCluster(w http.ResponseWriter, r *http.Request) {
	if s.Meta == nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", "meta store not configured")
		return
	}
	defer r.Body.Close()

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "read body: "+err.Error())
		return
	}
	var req CreateClusterRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "malformed JSON body")
		return
	}

	id := strings.TrimSpace(req.ID)
	backend := strings.TrimSpace(req.Backend)
	if !clusterIDPattern.MatchString(id) {
		writeJSONError(w, http.StatusBadRequest, "InvalidArgument",
			"id must match [a-z0-9-]{1,64}")
		return
	}
	if _, ok := clusterRegistryBackends[backend]; !ok {
		writeJSONError(w, http.StatusBadRequest, "InvalidArgument",
			"backend must be one of: rados, s3")
		return
	}
	specBytes := []byte(req.Spec)
	if len(specBytes) == 0 {
		writeJSONError(w, http.StatusBadRequest, "InvalidArgument",
			"spec is required and must be a non-empty JSON document")
		return
	}
	if !json.Valid(specBytes) {
		writeJSONError(w, http.StatusBadRequest, "InvalidArgument",
			"spec is not a well-formed JSON document")
		return
	}
	// Reject explicit JSON null / empty object — the consumer needs at
	// least one field to dial against.
	trimmed := strings.TrimSpace(string(specBytes))
	if trimmed == "null" || trimmed == "{}" {
		writeJSONError(w, http.StatusBadRequest, "InvalidArgument",
			"spec must be a non-empty JSON object")
		return
	}

	ctx := r.Context()
	owner := auth.FromContext(ctx).Owner
	s3api.SetAuditOverride(ctx, "admin:CreateCluster", "cluster:"+id, "-", owner)

	// Pre-check existence so we can return the right error code without
	// relying on PutCluster's CAS semantics (which target updates).
	if existing, gErr := s.Meta.GetCluster(ctx, id); gErr == nil && existing != nil {
		writeJSONError(w, http.StatusConflict, "ClusterAlreadyExists",
			"cluster with that id is already registered")
		return
	} else if gErr != nil && !errors.Is(gErr, meta.ErrClusterNotFound) {
		writeJSONError(w, http.StatusInternalServerError, "Internal", gErr.Error())
		return
	}

	entry := &meta.ClusterRegistryEntry{
		ID:      id,
		Backend: backend,
		Spec:    append([]byte(nil), specBytes...),
	}
	if err := s.Meta.PutCluster(ctx, entry); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, clusterToResponse(entry))
}

// handleDeleteCluster serves DELETE /admin/v1/storage/clusters/{id}.
// Returns 204 on success, 404 NoSuchCluster when missing, 409
// ClusterReferenced (with referenced_by list) when the running data
// backend still routes a storage class at this cluster.
func (s *Server) handleDeleteCluster(w http.ResponseWriter, r *http.Request) {
	if s.Meta == nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", "meta store not configured")
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "cluster id is required")
		return
	}

	ctx := r.Context()
	owner := auth.FromContext(ctx).Owner
	s3api.SetAuditOverride(ctx, "admin:DeleteCluster", "cluster:"+id, "-", owner)

	if checker, ok := s.Data.(data.ClusterReferenceChecker); ok {
		if refs := checker.ClassesUsingCluster(id); len(refs) > 0 {
			sort.Strings(refs)
			writeJSON(w, http.StatusConflict, ClusterReferencedResponse{
				Code:         "ClusterReferenced",
				Message:      "cluster is referenced by one or more storage classes",
				ReferencedBy: refs,
			})
			return
		}
	}

	err := s.Meta.DeleteCluster(ctx, id)
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, meta.ErrClusterNotFound):
		writeJSONError(w, http.StatusNotFound, "NoSuchCluster", "cluster not found")
	default:
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
	}
}

// clusterToResponse projects a meta.ClusterRegistryEntry onto the wire
// shape. Spec is forwarded verbatim — the registry layer never rewrites it.
func clusterToResponse(e *meta.ClusterRegistryEntry) ClusterEntryResponse {
	spec := json.RawMessage(e.Spec)
	if len(spec) == 0 {
		spec = json.RawMessage("null")
	}
	return ClusterEntryResponse{
		ID:        e.ID,
		Backend:   e.Backend,
		Spec:      spec,
		CreatedAt: e.CreatedAt.Unix(),
		UpdatedAt: epochOrZero(e.UpdatedAt),
		Version:   e.Version,
	}
}

func epochOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}
