package adminapi

import (
	"net/http"
	"sort"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/s3api"
)

// BucketReferenceEntry is one row in the bucket-references response. Weight
// echoes the bucket's Placement[clusterID]; ChunkCount / BytesUsed come from
// the live bucket_stats counter row (no manifest walk per request — the
// rebalance worker progress scan in US-003 covers the actual-distribution
// side and the bucket_stats counter is denormalised on every PUT/DELETE).
type BucketReferenceEntry struct {
	Name        string `json:"name"`
	Weight      int    `json:"weight"`
	ChunkCount  int64  `json:"chunk_count"`
	BytesUsed   int64  `json:"bytes_used"`
}

// BucketReferencesResponse is the wire shape returned by
// GET /admin/v1/clusters/{id}/bucket-references (US-006 drain-lifecycle).
// Buckets is the page slice ordered by chunk_count desc, then name asc.
// TotalBuckets is the matching-row count BEFORE pagination so the UI can
// render a "showing N of M" affordance. NextOffset is non-nil when more
// rows are available.
type BucketReferencesResponse struct {
	Buckets      []BucketReferenceEntry `json:"buckets"`
	TotalBuckets int                    `json:"total_buckets"`
	NextOffset   *int                   `json:"next_offset"`
}

// handleClusterBucketReferences serves GET /admin/v1/clusters/{id}/bucket-
// references. Filters ListBuckets on Placement[clusterID] > 0, joins each
// match with bucket_stats for chunk_count + bytes_used, sorts desc on
// chunk_count then asc on name, and paginates via ?limit=N&offset=M
// (default limit=100, max 1000).
func (s *Server) handleClusterBucketReferences(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "cluster id is required")
		return
	}
	if s.Meta == nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", "meta store not configured")
		return
	}
	if len(s.KnownClusters) > 0 {
		if _, ok := s.KnownClusters[id]; !ok {
			writeJSONError(w, http.StatusBadRequest, "UnknownCluster",
				"cluster id is not configured (check STRATA_RADOS_CLUSTERS / STRATA_S3_CLUSTERS)")
			return
		}
	}

	ctx := r.Context()
	owner := auth.FromContext(ctx).Owner
	s3api.SetAuditOverride(ctx, "admin:GetClusterBucketReferences", "cluster:"+id, "-", owner)

	q := r.URL.Query()
	limit := parseRange(q.Get("limit"), 100, 1, 1000)
	offset := parseRange(q.Get("offset"), 0, 0, 1<<30)

	buckets, err := s.Meta.ListBuckets(ctx, "")
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}

	matches := make([]BucketReferenceEntry, 0, len(buckets))
	for _, b := range buckets {
		policy, perr := s.Meta.GetBucketPlacement(ctx, b.Name)
		if perr != nil {
			// Bucket vanished between ListBuckets and GetBucketPlacement
			// (race with concurrent DeleteBucket) — skip it.
			continue
		}
		weight, ok := policy[id]
		if !ok || weight <= 0 {
			continue
		}
		stats, _ := s.Meta.GetBucketStats(ctx, b.ID)
		matches = append(matches, BucketReferenceEntry{
			Name:       b.Name,
			Weight:     weight,
			ChunkCount: stats.UsedObjects,
			BytesUsed:  stats.UsedBytes,
		})
	}

	sort.Slice(matches, func(i, j int) bool {
		if matches[i].ChunkCount != matches[j].ChunkCount {
			return matches[i].ChunkCount > matches[j].ChunkCount
		}
		return matches[i].Name < matches[j].Name
	})

	resp := BucketReferencesResponse{
		Buckets:      []BucketReferenceEntry{},
		TotalBuckets: len(matches),
	}
	if offset < len(matches) {
		end := min(offset+limit, len(matches))
		resp.Buckets = matches[offset:end]
		if end < len(matches) {
			next := end
			resp.NextOffset = &next
		}
	}
	writeJSON(w, http.StatusOK, resp)
}
