package adminapi

import (
	"context"
	"net/http"
	"sort"
	"strconv"

	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/promclient"
)

// handleBucketsList serves GET /admin/v1/buckets. Phase 1 stub.
func (s *Server) handleBucketsList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, BucketsListResponse{
		Buckets: []BucketSummary{},
		Total:   0,
	})
}

// handleBucketsTop serves GET /admin/v1/buckets/top. Aggregates the home-page
// "Top Buckets" widget. by=size (default) or by=requests; limit 1..100
// (default 10). Bucket size + object count come from a meta.Store walk;
// request_count_24h comes from PromQL when STRATA_PROMETHEUS_URL is set.
// MetricsAvailable=false signals the UI to render '—' for the request column
// and a "Metrics unavailable" warning under the card title.
func (s *Server) handleBucketsTop(w http.ResponseWriter, r *http.Request) {
	by := r.URL.Query().Get("by")
	if by == "" {
		by = "size"
	}
	if by != "size" && by != "requests" {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "by must be size or requests")
		return
	}
	limit := parseLimit(r.URL.Query().Get("limit"), 10)

	resp := BucketsTopResponse{Buckets: []BucketTop{}, MetricsAvailable: false}
	if s.Meta == nil {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	buckets, err := s.Meta.ListBuckets(r.Context(), "")
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	stats := make([]BucketTop, 0, len(buckets))
	for _, b := range buckets {
		size, count := bucketSizeAndCount(r.Context(), s.Meta, b)
		stats = append(stats, BucketTop{
			Name:        b.Name,
			SizeBytes:   size,
			ObjectCount: count,
		})
	}

	requestCounts, ok := queryBucketRequestCounts24h(r.Context(), s.Prom)
	if ok {
		resp.MetricsAvailable = true
		for i := range stats {
			stats[i].RequestCount24h = requestCounts[stats[i].Name]
		}
	}

	switch by {
	case "size":
		sort.SliceStable(stats, func(i, j int) bool {
			if stats[i].SizeBytes == stats[j].SizeBytes {
				return stats[i].Name < stats[j].Name
			}
			return stats[i].SizeBytes > stats[j].SizeBytes
		})
	case "requests":
		sort.SliceStable(stats, func(i, j int) bool {
			if stats[i].RequestCount24h == stats[j].RequestCount24h {
				return stats[i].Name < stats[j].Name
			}
			return stats[i].RequestCount24h > stats[j].RequestCount24h
		})
	}
	if len(stats) > limit {
		stats = stats[:limit]
	}
	resp.Buckets = stats
	writeJSON(w, http.StatusOK, resp)
}

// handleBucketGet serves GET /admin/v1/buckets/{bucket}. Phase 1 stub —
// always 404 until US-011 wires the meta.Store lookup.
func (s *Server) handleBucketGet(w http.ResponseWriter, r *http.Request) {
	writeJSONError(w, http.StatusNotFound, "NoSuchBucket", "bucket not found")
}

// handleObjectsList serves GET /admin/v1/buckets/{bucket}/objects. Phase 1 stub.
func (s *Server) handleObjectsList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, ObjectsListResponse{
		Objects:        []ObjectSummary{},
		CommonPrefixes: []string{},
	})
}

// bucketSizeAndCount walks ListObjects in pages and sums size + object count.
// Best-effort — failures return (0, 0) instead of erroring out the widget.
// Cap at 50_000 objects per bucket to bound the scan; widget is "top N", not
// authoritative inventory.
func bucketSizeAndCount(ctx context.Context, store meta.Store, b *meta.Bucket) (int64, int64) {
	const pageSize = 1000
	const maxObjects = 50_000
	var size, count int64
	marker := ""
	for count < maxObjects {
		res, err := store.ListObjects(ctx, b.ID, meta.ListOptions{
			Marker: marker,
			Limit:  pageSize,
		})
		if err != nil {
			return size, count
		}
		for _, o := range res.Objects {
			size += o.Size
			count++
		}
		if !res.Truncated || res.NextMarker == "" {
			break
		}
		marker = res.NextMarker
	}
	return size, count
}

// queryBucketRequestCounts24h asks Prometheus for per-bucket 24h request
// totals via the strata_http_requests_total counter, summed over the bucket
// label. Returns (counts, true) on success; (nil, false) when the metric or
// label is missing or Prometheus is unavailable — the UI falls back to '—'.
func queryBucketRequestCounts24h(ctx context.Context, prom *promclient.Client) (map[string]int64, bool) {
	if !prom.Available() {
		return nil, false
	}
	const expr = `sum by (bucket) (increase(strata_http_requests_total[24h]))`
	samples, err := prom.Query(ctx, expr)
	if err != nil {
		return nil, false
	}
	out := make(map[string]int64, len(samples))
	for _, s := range samples {
		name := s.Metric["bucket"]
		if name == "" {
			continue
		}
		out[name] = int64(s.Value)
	}
	return out, true
}

// parseLimit returns a sanitized limit value clamped to [1, 100].
func parseLimit(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		return def
	}
	if n > 100 {
		return 100
	}
	return n
}

