package adminapi

import (
	"context"
	"net/http"
	"sort"

	"github.com/danchupin/strata/internal/promclient"
)

// handleConsumersTop serves GET /admin/v1/consumers/top. Aggregates the
// home-page "Top Consumers" widget. by=requests (default) or by=bytes;
// limit 1..100 (default 10). Source is Prometheus only — when unavailable
// the response carries metrics_available=false and an empty list, and the
// UI renders "Metrics unavailable" instead of '—' rows.
func (s *Server) handleConsumersTop(w http.ResponseWriter, r *http.Request) {
	by := r.URL.Query().Get("by")
	if by == "" {
		by = "requests"
	}
	if by != "requests" && by != "bytes" {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "by must be requests or bytes")
		return
	}
	limit := parseLimit(r.URL.Query().Get("limit"), 10)

	resp := ConsumersTopResponse{Consumers: []ConsumerTop{}, MetricsAvailable: false}
	if !s.Prom.Available() {
		writeJSON(w, http.StatusOK, resp)
		return
	}

	consumers, ok := queryConsumerTotals24h(r.Context(), s.Prom)
	if !ok {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	resp.MetricsAvailable = true

	switch by {
	case "requests":
		sort.SliceStable(consumers, func(i, j int) bool {
			if consumers[i].RequestCount24h == consumers[j].RequestCount24h {
				return consumers[i].AccessKey < consumers[j].AccessKey
			}
			return consumers[i].RequestCount24h > consumers[j].RequestCount24h
		})
	case "bytes":
		sort.SliceStable(consumers, func(i, j int) bool {
			if consumers[i].Bytes24h == consumers[j].Bytes24h {
				return consumers[i].AccessKey < consumers[j].AccessKey
			}
			return consumers[i].Bytes24h > consumers[j].Bytes24h
		})
	}
	if len(consumers) > limit {
		consumers = consumers[:limit]
	}
	if s.Creds != nil {
		for i := range consumers {
			if cred, err := s.Creds.Lookup(r.Context(), consumers[i].AccessKey); err == nil && cred != nil {
				consumers[i].User = cred.Owner
			}
		}
	}
	resp.Consumers = consumers
	writeJSON(w, http.StatusOK, resp)
}

// queryConsumerTotals24h pulls per-access-key request counts and bytes from
// Prometheus over a 24h window. Returns (nil, false) when either query
// fails — the UI then surfaces "Metrics unavailable" rather than partial
// data.
func queryConsumerTotals24h(ctx context.Context, prom *promclient.Client) ([]ConsumerTop, bool) {
	const reqExpr = `sum by (access_key) (increase(strata_http_requests_total[24h]))`
	const byteExpr = `sum by (access_key) (increase(strata_http_bytes_total[24h]))`

	reqSamples, err := prom.Query(ctx, reqExpr)
	if err != nil {
		return nil, false
	}
	byteSamples, _ := prom.Query(ctx, byteExpr)

	totals := make(map[string]*ConsumerTop, len(reqSamples))
	for _, s := range reqSamples {
		ak := s.Metric["access_key"]
		if ak == "" {
			continue
		}
		c := totals[ak]
		if c == nil {
			c = &ConsumerTop{AccessKey: ak}
			totals[ak] = c
		}
		c.RequestCount24h = int64(s.Value)
	}
	for _, s := range byteSamples {
		ak := s.Metric["access_key"]
		if ak == "" {
			continue
		}
		c := totals[ak]
		if c == nil {
			c = &ConsumerTop{AccessKey: ak}
			totals[ak] = c
		}
		c.Bytes24h = int64(s.Value)
	}

	out := make([]ConsumerTop, 0, len(totals))
	for _, c := range totals {
		out = append(out, *c)
	}
	return out, true
}
