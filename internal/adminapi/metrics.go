package adminapi

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// metricSpec maps the public `metric` query parameter to a PromQL expression.
// The expression must contain the literal token __WINDOW__ which is replaced
// at request time with the rate window (typically the step). For metrics that
// produce multiple labelled series (latency layered with itself by quantile)
// the page calls the endpoint once per metric — each call returns a single
// series named `name` so the React layer can layer them in one chart.
type metricSpec struct {
	name string // series name returned to the UI
	expr string // PromQL with __WINDOW__ placeholder
}

var metricSpecs = map[string]metricSpec{
	"request_rate": {
		name: "request_rate",
		expr: `sum(rate(strata_http_requests_total[__WINDOW__]))`,
	},
	"latency_p50": {
		name: "p50",
		expr: `histogram_quantile(0.5, sum by (le) (rate(strata_http_request_duration_seconds_bucket[__WINDOW__])))`,
	},
	"latency_p95": {
		name: "p95",
		expr: `histogram_quantile(0.95, sum by (le) (rate(strata_http_request_duration_seconds_bucket[__WINDOW__])))`,
	},
	"latency_p99": {
		name: "p99",
		expr: `histogram_quantile(0.99, sum by (le) (rate(strata_http_request_duration_seconds_bucket[__WINDOW__])))`,
	},
	"error_rate": {
		name: "error_rate",
		expr: `sum(rate(strata_http_requests_total{code=~"5.."}[__WINDOW__])) / clamp_min(sum(rate(strata_http_requests_total[__WINDOW__])), 1)`,
	},
	"bytes_in": {
		name: "bytes_in",
		expr: `sum(rate(strata_http_bytes_in_total[__WINDOW__]))`,
	},
	"bytes_out": {
		name: "bytes_out",
		expr: `sum(rate(strata_http_bytes_out_total[__WINDOW__]))`,
	},
}

// metricNames returns the supported metric param values, sorted for stable
// error messages.
func metricNames() []string {
	out := make([]string, 0, len(metricSpecs))
	for k := range metricSpecs {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// rangeStepDefaults maps a range to a sensible default step when the client
// does not pass step= explicitly. Picks the resolution at which a 60-point
// chart looks dense without overwhelming Prometheus.
var rangeStepDefaults = map[time.Duration]time.Duration{
	15 * time.Minute: 30 * time.Second,
	1 * time.Hour:    1 * time.Minute,
	6 * time.Hour:    5 * time.Minute,
	24 * time.Hour:   15 * time.Minute,
	7 * 24 * time.Hour: 1 * time.Hour,
}

// parseDurationParam supports Go's time.ParseDuration plus "<N>d" for the 7d
// range that the spec calls out (Prometheus accepts d but stdlib does not).
func parseDurationParam(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty duration")
	}
	if days, ok := strings.CutSuffix(s, "d"); ok {
		// Convert "7d" → "168h" for time.ParseDuration.
		d, err := time.ParseDuration(days + "h")
		if err != nil {
			return 0, err
		}
		return d * 24, nil
	}
	return time.ParseDuration(s)
}

// defaultStep returns a reasonable step for a given range — exact match when
// the range hits one of the known breakpoints, else range/60 capped to [15s,
// 1h] so we don't fire excessive points for arbitrary ranges.
func defaultStep(rangeDur time.Duration) time.Duration {
	if step, ok := rangeStepDefaults[rangeDur]; ok {
		return step
	}
	step := rangeDur / 60
	if step < 15*time.Second {
		return 15 * time.Second
	}
	if step > time.Hour {
		return time.Hour
	}
	return step
}

// handleMetricsTimeseries serves GET /admin/v1/metrics/timeseries. Translates
// the public metric name + range/step into a PromQL range query, returns the
// resulting series in the {epoch_ms, value} shape recharts consumes.
//
// Degrades gracefully: when STRATA_PROMETHEUS_URL is unset OR the upstream
// Prometheus returns an error, the response is 200 with an empty series list
// and metrics_available=false. The UI renders a "Metrics unavailable —
// set STRATA_PROMETHEUS_URL" inline message in that case.
func (s *Server) handleMetricsTimeseries(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	metric := q.Get("metric")
	if metric == "" {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "metric is required")
		return
	}
	spec, ok := metricSpecs[metric]
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "BadRequest",
			fmt.Sprintf("unknown metric %q; supported: %s", metric, strings.Join(metricNames(), ", ")))
		return
	}

	rangeStr := q.Get("range")
	if rangeStr == "" {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "range is required (e.g. 15m, 1h, 6h, 24h, 7d)")
		return
	}
	rangeDur, err := parseDurationParam(rangeStr)
	if err != nil || rangeDur <= 0 {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", fmt.Sprintf("invalid range %q", rangeStr))
		return
	}

	stepStr := q.Get("step")
	var stepDur time.Duration
	if stepStr == "" {
		stepDur = defaultStep(rangeDur)
	} else {
		stepDur, err = parseDurationParam(stepStr)
		if err != nil || stepDur <= 0 {
			writeJSONError(w, http.StatusBadRequest, "BadRequest", fmt.Sprintf("invalid step %q", stepStr))
			return
		}
	}

	resp := MetricsTimeseriesResponse{Series: []MetricSeries{}, MetricsAvailable: false}
	if !s.Prom.Available() {
		writeJSON(w, http.StatusOK, resp)
		return
	}

	end := time.Now().UTC()
	start := end.Add(-rangeDur)
	expr := strings.ReplaceAll(spec.expr, "__WINDOW__", promDurationString(stepDur*4))

	series, err := s.Prom.QueryRange(r.Context(), expr, start, end, stepDur)
	if err != nil {
		s.Logger.Printf("adminapi: metrics/timeseries %s: %v", metric, err)
		writeJSON(w, http.StatusOK, resp)
		return
	}
	resp.MetricsAvailable = true

	out := MetricSeries{Name: spec.name, Points: make([]MetricPoint, 0)}
	for _, srs := range series {
		for _, p := range srs.Points {
			out.Points = append(out.Points, MetricPoint{float64(p.Timestamp.UnixMilli()), p.Value})
		}
	}
	if len(out.Points) > 0 {
		// Stable order by timestamp — recharts wants ascending x.
		sort.Slice(out.Points, func(i, j int) bool { return out.Points[i][0] < out.Points[j][0] })
	}
	resp.Series = []MetricSeries{out}
	writeJSON(w, http.StatusOK, resp)
}

// promDurationString formats a time.Duration as a PromQL range literal. Prom
// rejects fractional seconds with a unit suffix (e.g. "1.5m") and accepts "s",
// "m", "h", "d". We pick the largest whole-unit representation.
func promDurationString(d time.Duration) string {
	if d <= 0 {
		return "30s"
	}
	if d%time.Hour == 0 {
		return fmt.Sprintf("%dh", int64(d/time.Hour))
	}
	if d%time.Minute == 0 {
		return fmt.Sprintf("%dm", int64(d/time.Minute))
	}
	if d < time.Second {
		// Prometheus minimum reasonable resolution.
		return "1s"
	}
	return fmt.Sprintf("%ds", int64(d/time.Second))
}

