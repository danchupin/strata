package adminapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/promclient"
)

// replicationLagExprFmt is the PromQL backing GET /admin/v1/buckets/{bucket}/
// replication-lag (US-014). The metric is sampled per drain tick by the
// replicator worker as `now - oldest pending evt.EventTime` per source bucket
// (zero when the queue is empty).
const replicationLagExprFmt = `strata_replication_queue_age_seconds{bucket="%s"}`

const (
	defaultReplicationLagRange = time.Hour
	maxReplicationLagRange     = 24 * time.Hour
)

// BucketReplicationLagResponse is the wire shape returned by the handler.
// When `empty: true` the per-bucket Replication tab renders nothing — the
// bucket has no replication configuration. Callers must check Empty before
// reading Values.
type BucketReplicationLagResponse struct {
	Empty  bool                       `json:"empty,omitempty"`
	Reason string                     `json:"reason,omitempty"`
	Values []BucketReplicationLagPoint `json:"values,omitempty"`
}

type BucketReplicationLagPoint struct {
	TS    time.Time `json:"ts"`
	Value float64   `json:"value"`
}

// handleBucketReplicationLag serves GET /admin/v1/buckets/{bucket}/replication-lag.
// Resolves the bucket via meta.Store.GetBucket; returns `{empty: true}` when
// the bucket has no replication configuration so the UI tab can hide itself.
// 503 MetricsUnavailable when Prom is unset; 400 InvalidArgument on a
// non-positive `range`. Step auto-derives from the range so the wire payload
// stays bounded.
func (s *Server) handleBucketReplicationLag(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("bucket")
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "bucket name is required")
		return
	}
	stampAuditOverride(r, "admin:GetBucketReplicationLag", "buckets:replication-lag", name)

	rangeDur, err := parsePositiveDuration(r.URL.Query().Get("range"), defaultReplicationLagRange)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "InvalidArgument", "range must be a positive Go duration")
		return
	}
	if rangeDur > maxReplicationLagRange {
		rangeDur = maxReplicationLagRange
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

	if _, err := s.Meta.GetBucketReplication(r.Context(), b.ID); err != nil {
		if errors.Is(err, meta.ErrNoSuchReplication) {
			writeJSON(w, http.StatusOK, BucketReplicationLagResponse{
				Empty:  true,
				Reason: "no replication configuration",
			})
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}

	if !s.Prom.Available() {
		writeJSONError(w, http.StatusServiceUnavailable, "MetricsUnavailable",
			"Prometheus is not configured (STRATA_PROMETHEUS_URL is empty)")
		return
	}

	resp, err := buildBucketReplicationLag(r.Context(), s.Prom, name, time.Now(), rangeDur)
	if err != nil {
		if errors.Is(err, promclient.ErrUnavailable) {
			writeJSONError(w, http.StatusServiceUnavailable, "MetricsUnavailable", err.Error())
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func buildBucketReplicationLag(ctx context.Context, prom *promclient.Client, bucket string, now time.Time, rangeDur time.Duration) (BucketReplicationLagResponse, error) {
	end := now
	start := end.Add(-rangeDur)
	step := replicationLagStep(rangeDur)
	expr := fmt.Sprintf(replicationLagExprFmt, bucket)
	series, err := prom.QueryRange(ctx, expr, start, end, step)
	if err != nil {
		return BucketReplicationLagResponse{}, err
	}
	// The metric is single-series per bucket (label key `bucket`); take the
	// first matching series. Empty matrix → flatline (UI renders nothing).
	out := BucketReplicationLagResponse{Values: []BucketReplicationLagPoint{}}
	if len(series) > 0 {
		for _, p := range series[0].Points {
			out.Values = append(out.Values, BucketReplicationLagPoint{TS: p.Timestamp, Value: p.Value})
		}
	}
	return out, nil
}

// replicationLagStep auto-derives the PromQL step from the requested range so
// the returned wire payload stays bounded (~60-180 points). Mirrors the UI
// dropdown options: 15m → 5s? no — we want the same buckets the heatmaps use.
// 15m → 15s, 1h → 1m, 6h → 5m, 24h → 30m.
func replicationLagStep(rangeDur time.Duration) time.Duration {
	switch {
	case rangeDur <= 15*time.Minute:
		return 15 * time.Second
	case rangeDur <= time.Hour:
		return time.Minute
	case rangeDur <= 6*time.Hour:
		return 5 * time.Minute
	default:
		return 30 * time.Minute
	}
}
