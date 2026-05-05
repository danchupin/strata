package adminapi

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/danchupin/strata/internal/promclient"
)

// hotBucketsExpr is the PromQL backing GET /admin/v1/diagnostics/hot-buckets.
// `sum by (bucket) (rate(strata_http_requests_total[1m]))` aggregates request
// rate across the gateway fleet per bucket; the range query then evaluates it
// at every `step` over the requested window.
const hotBucketsExpr = `sum by (bucket) (rate(strata_http_requests_total[1m]))`

const (
	defaultHotBucketsRange = time.Hour
	defaultHotBucketsStep  = time.Minute
	hotBucketsTopN         = 50
	hotBucketsCacheTTL     = 30 * time.Second
)

// HotBucketsResponse mirrors the wire shape `{matrix: [{bucket, values: [{ts, value}]}]}`
// emitted by the handler. Keep the JSON tags stable — the heatmap UI in
// US-008 reads this verbatim.
type HotBucketsResponse struct {
	Matrix []HotBucketSeries `json:"matrix"`
}

type HotBucketSeries struct {
	Bucket string           `json:"bucket"`
	Values []HotBucketPoint `json:"values"`
}

type HotBucketPoint struct {
	TS    time.Time `json:"ts"`
	Value float64   `json:"value"`
}

// handleDiagnosticsHotBuckets serves GET /admin/v1/diagnostics/hot-buckets
// (US-007). Defaults: range=1h, step=1m. 503 MetricsUnavailable when Prom is
// not configured. A 30s in-process cache keyed on (range, step) absorbs
// burst polling from multiple operator viewers.
func (s *Server) handleDiagnosticsHotBuckets(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	rangeDur, err := parsePositiveDuration(q.Get("range"), defaultHotBucketsRange)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "InvalidArgument", "range must be a positive Go duration")
		return
	}
	stepDur, err := parsePositiveDuration(q.Get("step"), defaultHotBucketsStep)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "InvalidArgument", "step must be a positive Go duration")
		return
	}

	stampAuditOverride(r, "admin:GetHotBuckets", "diagnostics:hot-buckets", "")

	if !s.Prom.Available() {
		writeJSONError(w, http.StatusServiceUnavailable, "MetricsUnavailable",
			"Prometheus is not configured (STRATA_PROMETHEUS_URL is empty)")
		return
	}

	cache := s.hotBuckets()
	if cached, ok := cache.get(rangeDur, stepDur); ok {
		writeJSON(w, http.StatusOK, cached)
		return
	}

	resp, err := buildHotBuckets(r.Context(), s.Prom, time.Now(), rangeDur, stepDur)
	if err != nil {
		if errors.Is(err, promclient.ErrUnavailable) {
			writeJSONError(w, http.StatusServiceUnavailable, "MetricsUnavailable", err.Error())
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	cache.set(rangeDur, stepDur, resp)
	writeJSON(w, http.StatusOK, resp)
}

// buildHotBuckets runs the PromQL range query, trims to top-N by total
// request count, and shapes the wire payload. now is injected so tests can
// pin the result window.
func buildHotBuckets(ctx context.Context, prom *promclient.Client, now time.Time, rangeDur, step time.Duration) (HotBucketsResponse, error) {
	end := now
	start := end.Add(-rangeDur)
	series, err := prom.QueryRange(ctx, hotBucketsExpr, start, end, step)
	if err != nil {
		return HotBucketsResponse{}, err
	}

	type ranked struct {
		bucket string
		total  float64
		points []HotBucketPoint
	}
	out := make([]ranked, 0, len(series))
	for _, s := range series {
		bucket := s.Metric["bucket"]
		if bucket == "" {
			continue
		}
		var total float64
		points := make([]HotBucketPoint, 0, len(s.Points))
		for _, p := range s.Points {
			points = append(points, HotBucketPoint{TS: p.Timestamp, Value: p.Value})
			total += p.Value
		}
		out = append(out, ranked{bucket: bucket, total: total, points: points})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].total == out[j].total {
			return out[i].bucket < out[j].bucket
		}
		return out[i].total > out[j].total
	})
	if len(out) > hotBucketsTopN {
		out = out[:hotBucketsTopN]
	}
	resp := HotBucketsResponse{Matrix: make([]HotBucketSeries, 0, len(out))}
	for _, r := range out {
		resp.Matrix = append(resp.Matrix, HotBucketSeries{Bucket: r.bucket, Values: r.points})
	}
	return resp, nil
}

// parsePositiveDuration parses a Go duration string. Empty falls back to
// fallback. Zero or negative durations are rejected.
func parsePositiveDuration(raw string, fallback time.Duration) (time.Duration, error) {
	if raw == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, err
	}
	if d <= 0 {
		return 0, errors.New("must be positive")
	}
	return d, nil
}

// hotBuckets returns the lazily-initialised TTL cache for hot-buckets
// responses. Concurrent first-callers race harmlessly — the loser's cache
// is discarded and only the winner's instance is reused.
func (s *Server) hotBuckets() *hotBucketsCache {
	s.hotBucketsMu.Lock()
	defer s.hotBucketsMu.Unlock()
	if s.hotBucketsCacheVal == nil {
		s.hotBucketsCacheVal = &hotBucketsCache{ttl: hotBucketsCacheTTL, now: time.Now}
	}
	return s.hotBucketsCacheVal
}

// hotBucketsCache is a tiny TTL cache keyed on (range, step) durations.
// Capacity is implicit: the keyspace is bounded by the UI's range/step
// dropdowns (typically 4–6 entries), so no LRU eviction is needed beyond
// expiry. now is overridable for tests.
type hotBucketsCache struct {
	mu      sync.Mutex
	entries map[string]hotBucketsCacheEntry
	ttl     time.Duration
	now     func() time.Time
}

type hotBucketsCacheEntry struct {
	expires time.Time
	payload HotBucketsResponse
}

func (c *hotBucketsCache) key(rangeDur, step time.Duration) string {
	return rangeDur.String() + "|" + step.String()
}

func (c *hotBucketsCache) get(rangeDur, step time.Duration) (HotBucketsResponse, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[c.key(rangeDur, step)]
	if !ok || c.now().After(e.expires) {
		return HotBucketsResponse{}, false
	}
	return e.payload, true
}

func (c *hotBucketsCache) set(rangeDur, step time.Duration, payload HotBucketsResponse) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]hotBucketsCacheEntry)
	}
	c.entries[c.key(rangeDur, step)] = hotBucketsCacheEntry{
		expires: c.now().Add(c.ttl),
		payload: payload,
	}
}
