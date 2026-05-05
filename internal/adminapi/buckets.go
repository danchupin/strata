package adminapi

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/promclient"
)

// bucketsListSortColumns enumerates the columns the React table can sort on.
// Anything else triggers a 400 — callers must explicitly choose a known column.
var bucketsListSortColumns = map[string]struct{}{
	"name":         {},
	"owner":        {},
	"created":      {},
	"size":         {},
	"object_count": {},
}

// handleBucketsList serves GET /admin/v1/buckets — the Buckets page list (US-010).
// Query params: query (case-insensitive substring on name), sort (name|owner|
// created|size|object_count), order (asc|desc, default desc for created and
// size, asc for name/owner), page (1-based, default 1), page_size (1..500,
// default 50). Returns BucketsListResponse{buckets, total} where total is the
// matching-row count BEFORE pagination so the UI can render page-count chips.
func (s *Server) handleBucketsList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sortCol := strings.ToLower(strings.TrimSpace(q.Get("sort")))
	if sortCol == "" {
		sortCol = "created"
	}
	if _, ok := bucketsListSortColumns[sortCol]; !ok {
		writeJSONError(w, http.StatusBadRequest, "BadRequest",
			"sort must be one of name, owner, created, size, object_count")
		return
	}
	order := strings.ToLower(strings.TrimSpace(q.Get("order")))
	switch order {
	case "":
		if sortCol == "name" || sortCol == "owner" {
			order = "asc"
		} else {
			order = "desc"
		}
	case "asc", "desc":
	default:
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "order must be asc or desc")
		return
	}
	page := parsePositive(q.Get("page"), 1)
	pageSize := parseRange(q.Get("page_size"), 50, 1, 500)
	query := strings.TrimSpace(q.Get("query"))
	queryLower := strings.ToLower(query)

	resp := BucketsListResponse{Buckets: []BucketSummary{}, Total: 0}
	if s.Meta == nil {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	buckets, err := s.Meta.ListBuckets(r.Context(), "")
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}

	entries := make([]bucketsListEntry, 0, len(buckets))
	for _, b := range buckets {
		if queryLower != "" && !strings.Contains(strings.ToLower(b.Name), queryLower) {
			continue
		}
		entries = append(entries, bucketsListEntry{
			bucket: b,
			summary: BucketSummary{
				Name:      b.Name,
				Owner:     b.Owner,
				Region:    s.Region,
				CreatedAt: b.CreatedAt.Unix(),
			},
		})
	}
	resp.Total = len(entries)

	// When sorting by size or object_count, walk every matched bucket up front
	// — the sort key needs the value. For other sorts we defer the walk to the
	// paginated slice so a 50-row page costs at most 50 ListObjects walks.
	if sortCol == "size" || sortCol == "object_count" {
		for i := range entries {
			size, count := bucketSizeAndCount(r.Context(), s.Meta, entries[i].bucket)
			entries[i].summary.SizeBytes = size
			entries[i].summary.ObjectCount = count
		}
	}

	sortBucketsListEntries(entries, sortCol, order)

	start := (page - 1) * pageSize
	if start < 0 || start >= len(entries) {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	end := start + pageSize
	if end > len(entries) {
		end = len(entries)
	}
	paged := entries[start:end]

	rows := make([]BucketSummary, 0, len(paged))
	for i := range paged {
		row := paged[i].summary
		if sortCol != "size" && sortCol != "object_count" {
			row.SizeBytes, row.ObjectCount = bucketSizeAndCount(r.Context(), s.Meta, paged[i].bucket)
		}
		rows = append(rows, row)
	}

	resp.Buckets = rows
	writeJSON(w, http.StatusOK, resp)
}

// bucketsListEntry pairs a meta.Bucket pointer with the partial BucketSummary
// projected from it. Sort + paginate operate on entries; the bucket pointer
// stays attached so the post-pagination loop can still walk size/object_count
// for the displayed page when the sort column did not require it.
type bucketsListEntry struct {
	bucket  *meta.Bucket
	summary BucketSummary
}

// sortBucketsListEntries orders entries in-place by the requested column.
// The secondary tie-break is always Name ASC regardless of the primary
// direction, so two rows that compare equal on the primary key remain in a
// predictable alphabetical sub-order. The "created" branch sorts on the
// bucket's full-precision time.Time rather than the seconds-truncated
// summary.CreatedAt, so sub-second-apart buckets still compare distinctly.
func sortBucketsListEntries(entries []bucketsListEntry, col, order string) {
	desc := order == "desc"
	primary := func(a, b bucketsListEntry) int {
		switch col {
		case "name":
			return cmpString(a.summary.Name, b.summary.Name)
		case "owner":
			return cmpString(a.summary.Owner, b.summary.Owner)
		case "size":
			return cmpInt64(a.summary.SizeBytes, b.summary.SizeBytes)
		case "object_count":
			return cmpInt64(a.summary.ObjectCount, b.summary.ObjectCount)
		default: // "created"
			ai := a.bucket.CreatedAt.UnixNano()
			bi := b.bucket.CreatedAt.UnixNano()
			return cmpInt64(ai, bi)
		}
	}
	sort.SliceStable(entries, func(i, j int) bool {
		c := primary(entries[i], entries[j])
		if desc {
			c = -c
		}
		if c != 0 {
			return c < 0
		}
		return entries[i].summary.Name < entries[j].summary.Name
	})
}

func cmpString(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

func cmpInt64(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// parsePositive returns n parsed from s when n >= 1, else def.
func parsePositive(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		return def
	}
	return n
}

// parseRange returns n parsed from s, clamped to [lo, hi]. Empty/invalid → def.
func parseRange(s string, def, lo, hi int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < lo {
		return def
	}
	if n > hi {
		return hi
	}
	return n
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

// handleBucketGet serves GET /admin/v1/buckets/{bucket} — the bucket detail
// header (US-011). Looks up the bucket via meta.Store; computes size_bytes
// and object_count via a bounded ListObjects walk (capped at 50_000 objects,
// matching the buckets/top widget cost ceiling). Versioning maps the
// meta-store enum (Disabled|Enabled|Suspended) to the operator-facing label
// (Off|Enabled|Suspended). Returns 404 NoSuchBucket when missing.
func (s *Server) handleBucketGet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("bucket")
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "bucket name is required")
		return
	}
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
	size, count := bucketSizeAndCount(r.Context(), s.Meta, b)
	writeJSON(w, http.StatusOK, BucketDetail{
		Name:                  b.Name,
		Owner:                 b.Owner,
		Region:                s.Region,
		CreatedAt:             b.CreatedAt.Unix(),
		Versioning:            versioningLabel(b.Versioning),
		ObjectLock:            b.ObjectLockEnabled,
		SizeBytes:             size,
		ObjectCount:           count,
		BackendPresign:        b.BackendPresign,
		ShardCount:            b.ShardCount,
		ReplicationConfigured: bucketHasReplication(r.Context(), s.Meta, b.ID),
	})
}

// bucketHasReplication probes meta.Store.GetBucketReplication; returns false
// on ErrNoSuchReplication (the common case) and on any unexpected error so
// the bucket detail handler never 500s on a transient meta error here. The
// per-bucket Replication tab (US-014) gates on this flag.
func bucketHasReplication(ctx context.Context, store meta.Store, bucketID uuid.UUID) bool {
	if store == nil {
		return false
	}
	blob, err := store.GetBucketReplication(ctx, bucketID)
	if err != nil {
		return false
	}
	return len(blob) > 0
}

// versioningLabel maps the meta.Versioning* enum to the operator-facing
// label the React badge renders. Anything unrecognised returns the raw
// value so future states surface verbatim instead of being silently masked.
func versioningLabel(state string) string {
	switch state {
	case meta.VersioningDisabled, "":
		return "Off"
	case meta.VersioningEnabled:
		return "Enabled"
	case meta.VersioningSuspended:
		return "Suspended"
	default:
		return state
	}
}

// handleObjectsList serves GET /admin/v1/buckets/{bucket}/objects — the
// read-only object browser (US-011). Query params: prefix (folder filter),
// marker (continuation token), delimiter (default '/' so the browser sees
// folders; pass an empty string to get a flat listing), page_size (1..1000,
// default 100). Returns 404 when the bucket is missing.
func (s *Server) handleObjectsList(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("bucket")
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "bucket name is required")
		return
	}
	q := r.URL.Query()
	prefix := q.Get("prefix")
	marker := q.Get("marker")
	delimiter := "/"
	if q.Has("delimiter") {
		// Empty string is a meaningful value here — flat listing without
		// folder collapsing — so honour an explicit delimiter= query param
		// and only fall back to "/" when the param is absent.
		delimiter = q.Get("delimiter")
	}
	pageSize := parseRange(q.Get("page_size"), 100, 1, 1000)
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
	res, err := s.Meta.ListObjects(r.Context(), b.ID, meta.ListOptions{
		Prefix:    prefix,
		Delimiter: delimiter,
		Marker:    marker,
		Limit:     pageSize,
	})
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	out := ObjectsListResponse{
		Objects:        make([]ObjectSummary, 0, len(res.Objects)),
		CommonPrefixes: append([]string{}, res.CommonPrefixes...),
		NextMarker:     res.NextMarker,
		IsTruncated:    res.Truncated,
	}
	if out.CommonPrefixes == nil {
		out.CommonPrefixes = []string{}
	}
	for _, o := range res.Objects {
		out.Objects = append(out.Objects, ObjectSummary{
			Key:          o.Key,
			Size:         o.Size,
			LastModified: o.Mtime.Unix(),
			ETag:         o.ETag,
			StorageClass: o.StorageClass,
		})
	}
	writeJSON(w, http.StatusOK, out)
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

