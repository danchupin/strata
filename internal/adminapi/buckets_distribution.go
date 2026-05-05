package adminapi

import (
	"errors"
	"net/http"
	"sort"

	"github.com/danchupin/strata/internal/meta"
)

// BucketDistributionResponse is the wire shape returned by
// GET /admin/v1/buckets/{bucket}/distribution (US-013). Each row is one shard
// of the bucket's sharded `objects` table partition with the live (latest
// non-delete-marker) byte and object totals — same data the bucketstats
// sampler emits via the `strata_bucket_shard_{bytes,objects}` gauges.
type BucketDistributionResponse struct {
	Shards []BucketShardStat `json:"shards"`
}

type BucketShardStat struct {
	Shard   int   `json:"shard"`
	Bytes   int64 `json:"bytes"`
	Objects int64 `json:"objects"`
}

// handleBucketDistribution serves GET /admin/v1/buckets/{bucket}/distribution.
// Reads per-shard byte/object totals via meta.Store.SampleBucketShardStats —
// the canonical surface from US-012 backing the bucketstats Prom gauges. The
// response always carries one row per shard ID 0..N-1 (zero-filled when a
// shard has no live objects) so the UI BarChart x-axis is contiguous.
// 404 NoSuchBucket on missing bucket; 500 Internal on any meta error.
func (s *Server) handleBucketDistribution(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("bucket")
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "bucket name is required")
		return
	}
	stampAuditOverride(r, "admin:GetBucketDistribution", "buckets:distribution", name)
	if s.Meta == nil {
		writeJSONError(w, http.StatusNotFound, "NoSuchBucket", "bucket not found")
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
	stats, err := s.Meta.SampleBucketShardStats(r.Context(), b.ID, b.ShardCount)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	rows := make([]BucketShardStat, 0, b.ShardCount)
	for shard := range b.ShardCount {
		st := stats[shard]
		rows = append(rows, BucketShardStat{
			Shard:   shard,
			Bytes:   st.Bytes,
			Objects: st.Objects,
		})
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Shard < rows[j].Shard })
	writeJSON(w, http.StatusOK, BucketDistributionResponse{Shards: rows})
}
