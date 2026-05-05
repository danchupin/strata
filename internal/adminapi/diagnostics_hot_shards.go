package adminapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/danchupin/strata/internal/promclient"
)

// hotShardsExprFmt is the PromQL backing GET /admin/v1/diagnostics/hot-shards/
// {bucket}. Per-shard rate of LWT (CAS) conflicts on the `objects` table — the
// hot-spot signal the Hot Shards heatmap renders. The bucket label is
// substituted server-side; the metric is emitted by SetObjectStorage on
// applied=false (US-009 prerequisite).
const hotShardsExprFmt = `sum by (shard) (rate(strata_cassandra_lwt_conflicts_total{bucket="%s"}[1m]))`

const (
	defaultHotShardsRange = time.Hour
	defaultHotShardsStep  = time.Minute
	hotShardsCacheTTL     = 30 * time.Second
)

// HotShardsResponse is the wire shape returned by the handler. When
// `empty: true` the heatmap UI renders the s3-backend explainer card and the
// matrix is nil — callers must check Empty before reading Matrix.
type HotShardsResponse struct {
	Empty  bool             `json:"empty,omitempty"`
	Reason string           `json:"reason,omitempty"`
	Matrix []HotShardSeries `json:"matrix,omitempty"`
}

type HotShardSeries struct {
	Shard  string          `json:"shard"`
	Values []HotShardPoint `json:"values"`
}

type HotShardPoint struct {
	TS    time.Time `json:"ts"`
	Value float64   `json:"value"`
}

// handleDiagnosticsHotShards serves GET /admin/v1/diagnostics/hot-shards/{bucket}
// (US-009). On the s3-over-s3 data backend the response short-circuits with
// `{empty: true, reason: ...}` — there are no shards to rank. Otherwise:
// PromQL roundtrip with a 30s in-process cache keyed on (bucket, range, step).
// 503 MetricsUnavailable when STRATA_PROMETHEUS_URL is unset.
func (s *Server) handleDiagnosticsHotShards(w http.ResponseWriter, r *http.Request) {
	bucket := r.PathValue("bucket")
	if bucket == "" {
		writeJSONError(w, http.StatusBadRequest, "InvalidArgument", "bucket path segment is required")
		return
	}
	stampAuditOverride(r, "admin:GetHotShards", "diagnostics:hot-shards", bucket)

	q := r.URL.Query()
	rangeDur, err := parsePositiveDuration(q.Get("range"), defaultHotShardsRange)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "InvalidArgument", "range must be a positive Go duration")
		return
	}
	stepDur, err := parsePositiveDuration(q.Get("step"), defaultHotShardsStep)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "InvalidArgument", "step must be a positive Go duration")
		return
	}

	// s3-over-s3 stores objects 1:1 — no shards exist, so the heatmap is not
	// meaningful. Short-circuit BEFORE the Prom-availability check so an
	// s3-backend cluster without Prom still gets a clean empty-state.
	if s.DataBackend == "s3" {
		writeJSON(w, http.StatusOK, HotShardsResponse{
			Empty:  true,
			Reason: "s3-over-s3 stores objects 1:1, no shards",
		})
		return
	}

	if !s.Prom.Available() {
		writeJSONError(w, http.StatusServiceUnavailable, "MetricsUnavailable",
			"Prometheus is not configured (STRATA_PROMETHEUS_URL is empty)")
		return
	}

	cache := s.hotShards()
	if cached, ok := cache.get(bucket, rangeDur, stepDur); ok {
		writeJSON(w, http.StatusOK, cached)
		return
	}

	resp, err := buildHotShards(r.Context(), s.Prom, bucket, time.Now(), rangeDur, stepDur)
	if err != nil {
		if errors.Is(err, promclient.ErrUnavailable) {
			writeJSONError(w, http.StatusServiceUnavailable, "MetricsUnavailable", err.Error())
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	cache.set(bucket, rangeDur, stepDur, resp)
	writeJSON(w, http.StatusOK, resp)
}

// buildHotShards runs the per-bucket PromQL range query and shapes the wire
// payload. now is injected so tests can pin the result window.
func buildHotShards(ctx context.Context, prom *promclient.Client, bucket string, now time.Time, rangeDur, step time.Duration) (HotShardsResponse, error) {
	end := now
	start := end.Add(-rangeDur)
	expr := fmt.Sprintf(hotShardsExprFmt, bucket)
	series, err := prom.QueryRange(ctx, expr, start, end, step)
	if err != nil {
		return HotShardsResponse{}, err
	}

	type ranked struct {
		shard  string
		total  float64
		points []HotShardPoint
	}
	out := make([]ranked, 0, len(series))
	for _, s := range series {
		shard := s.Metric["shard"]
		if shard == "" {
			continue
		}
		var total float64
		points := make([]HotShardPoint, 0, len(s.Points))
		for _, p := range s.Points {
			points = append(points, HotShardPoint{TS: p.Timestamp, Value: p.Value})
			total += p.Value
		}
		out = append(out, ranked{shard: shard, total: total, points: points})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].total == out[j].total {
			return out[i].shard < out[j].shard
		}
		return out[i].total > out[j].total
	})
	resp := HotShardsResponse{Matrix: make([]HotShardSeries, 0, len(out))}
	for _, r := range out {
		resp.Matrix = append(resp.Matrix, HotShardSeries{Shard: r.shard, Values: r.points})
	}
	return resp, nil
}

// hotShards returns the lazily-initialised TTL cache for hot-shards
// responses. Mirrors hotBuckets() — same lock pattern, same TTL.
func (s *Server) hotShards() *hotShardsCache {
	s.hotShardsMu.Lock()
	defer s.hotShardsMu.Unlock()
	if s.hotShardsCacheVal == nil {
		s.hotShardsCacheVal = &hotShardsCache{ttl: hotShardsCacheTTL, now: time.Now}
	}
	return s.hotShardsCacheVal
}

// hotShardsCache is a per-(bucket, range, step) TTL cache. Cardinality is
// bounded by the number of buckets the operator views in 30s — small in
// practice, no LRU needed.
type hotShardsCache struct {
	mu      sync.Mutex
	entries map[string]hotShardsCacheEntry
	ttl     time.Duration
	now     func() time.Time
}

type hotShardsCacheEntry struct {
	expires time.Time
	payload HotShardsResponse
}

func (c *hotShardsCache) key(bucket string, rangeDur, step time.Duration) string {
	return bucket + "|" + rangeDur.String() + "|" + step.String()
}

func (c *hotShardsCache) get(bucket string, rangeDur, step time.Duration) (HotShardsResponse, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[c.key(bucket, rangeDur, step)]
	if !ok || c.now().After(e.expires) {
		return HotShardsResponse{}, false
	}
	return e.payload, true
}

func (c *hotShardsCache) set(bucket string, rangeDur, step time.Duration, payload HotShardsResponse) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]hotShardsCacheEntry)
	}
	c.entries[c.key(bucket, rangeDur, step)] = hotShardsCacheEntry{
		expires: c.now().Add(c.ttl),
		payload: payload,
	}
}
