package admin

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/danchupin/strata/internal/promclient"
)

// cmdSLOReport queries Prometheus for the SLO actuals + top-N
// (5xx / slow) path tables defined in /operate/slo.md and renders a
// markdown (or JSON) compliance report. Preserves the single-binary
// invariant — this lives inside the `strata` binary as a subcommand,
// not a bash+curl+jq external script.
func (a *app) cmdSLOReport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("admin slo-report", flag.ContinueOnError)
	fs.SetOutput(a.err)
	promURL := fs.String("prometheus-url", envOrDefault("STRATA_PROMETHEUS_URL", "http://localhost:9090"), "Prometheus base URL")
	amURL := fs.String("alertmanager-url", os.Getenv("STRATA_ALERTMANAGER_URL"), "optional Alertmanager base URL; when set, the report lists active burn-rate alerts")
	window := fs.String("window", "7d", "report window — one of 7d, 30d, 90d")
	out := fs.String("out", "", "output file path (default stdout)")
	format := fs.String("format", "markdown", "output format — markdown or json")
	if err := fs.Parse(args); err != nil {
		return ErrUsage
	}
	if err := validateWindow(*window); err != nil {
		return err
	}
	if *format != "markdown" && *format != "json" {
		return fmt.Errorf("--format must be markdown or json, got %q", *format)
	}

	pc := promclient.New(*promURL)
	rep, err := buildSLOReport(ctx, pc, *amURL, *window, http.DefaultClient)
	if err != nil {
		return fmt.Errorf("build report: %w", err)
	}

	w := a.out
	if *out != "" {
		f, ferr := os.Create(*out)
		if ferr != nil {
			return fmt.Errorf("create out: %w", ferr)
		}
		defer f.Close()
		w = f
	}
	switch *format {
	case "json":
		return writeJSON(w, rep)
	default:
		return renderSLOMarkdown(w, rep)
	}
}

// sloReport is the typed payload backing both markdown + JSON output.
type sloReport struct {
	GeneratedAt    time.Time      `json:"generated_at"`
	Window         string         `json:"window"`
	PrometheusURL  string         `json:"prometheus_url"`
	SLOs           []sloRow       `json:"slos"`
	Top5xxPaths    []pathRate     `json:"top_5xx_paths"`
	TopSlowPaths   []pathLatency  `json:"top_slow_paths"`
	BurnRateAlerts []alertEntry   `json:"burn_rate_alerts,omitempty"`
	BurnRateError  string         `json:"burn_rate_error,omitempty"`
}

type sloRow struct {
	Name   string  `json:"name"`
	Target float64 `json:"target"`
	Actual float64 `json:"actual"`
	Status string  `json:"status"` // ok / warning / breached
	Unit   string  `json:"unit"`
	Note   string  `json:"note,omitempty"`
}

type pathRate struct {
	Path  string  `json:"path"`
	Value float64 `json:"value"`
}

type pathLatency struct {
	Path  string  `json:"path"`
	P99   float64 `json:"p99_seconds"`
}

type alertEntry struct {
	Alertname  string            `json:"alertname"`
	Labels     map[string]string `json:"labels"`
	StartsAt   time.Time         `json:"starts_at"`
	State      string            `json:"state"`
}

func validateWindow(w string) error {
	switch w {
	case "7d", "30d", "90d":
		return nil
	default:
		return fmt.Errorf("--window must be 7d, 30d, or 90d, got %q", w)
	}
}

// buildSLOReport is split off so tests can drive it with an httptest
// Prometheus stand-in and (optionally) an Alertmanager stand-in.
func buildSLOReport(ctx context.Context, pc *promclient.Client, amURL, window string, amHTTP *http.Client) (*sloReport, error) {
	if !pc.Available() {
		return nil, errors.New("prometheus url unconfigured")
	}
	rep := &sloReport{
		GeneratedAt:   time.Now().UTC(),
		Window:        window,
		PrometheusURL: pc.BaseURL,
	}

	availTarget, _ := queryScalar(ctx, pc, "strata:slo_availability:target")
	latGetPutTarget, _ := queryScalar(ctx, pc, "strata:slo_latency_get_put_seconds:target")
	durTarget, _ := queryScalar(ctx, pc, "strata:slo_durability_error_rate:target")

	availActual, availErr := queryScalar(ctx, pc, fmt.Sprintf("avg_over_time(strata:availability:ratio_rate5m[%s])", window))
	latActual, latErr := queryScalar(ctx, pc, fmt.Sprintf("avg_over_time(strata:latency_get_put:p99_rate5m[%s])", window))
	durActual, durErr := queryScalar(ctx, pc, fmt.Sprintf("sum(increase(strata_gc_terminal_ack_total{reason!=\"enoent\",reason!=\"ok\"}[%s]))", window))

	rep.SLOs = []sloRow{
		{
			Name:   "Availability (5xx-free request ratio)",
			Target: availTarget,
			Actual: availActual,
			Unit:   "ratio",
			Status: statusForRatioSLO(availActual, availTarget),
			Note:   firstErr("no availability samples", availErr),
		},
		{
			Name:   "Latency p99 GET/PUT (seconds)",
			Target: latGetPutTarget,
			Actual: latActual,
			Unit:   "seconds",
			Status: statusForLatencySLO(latActual, latGetPutTarget),
			Note:   firstErr("no latency samples", latErr),
		},
		{
			Name:   "Durability (non-OK terminal GC acks)",
			Target: durTarget,
			Actual: durActual,
			Unit:   "events/window",
			Status: statusForDurabilitySLO(durActual, durTarget),
			Note:   firstErr("no GC terminal acks", durErr),
		},
	}

	rep.Top5xxPaths, _ = queryTopPathRate(ctx, pc,
		fmt.Sprintf("topk(5, sum by (path) (rate(strata_http_requests_total{code=~\"5..\"}[%s])))", window))
	rep.TopSlowPaths, _ = queryTopPathLatency(ctx, pc,
		fmt.Sprintf("topk(5, histogram_quantile(0.99, sum by (le, path) (rate(strata_http_request_duration_seconds_bucket[%s]))))", window))

	if amURL != "" {
		alerts, err := fetchBurnRateAlerts(ctx, amHTTP, amURL)
		if err != nil {
			rep.BurnRateError = err.Error()
		} else {
			rep.BurnRateAlerts = alerts
		}
	}
	return rep, nil
}

func queryScalar(ctx context.Context, pc *promclient.Client, expr string) (float64, error) {
	samples, err := pc.Query(ctx, expr)
	if err != nil {
		return 0, err
	}
	if len(samples) == 0 {
		return 0, errors.New("empty result")
	}
	return samples[0].Value, nil
}

func queryTopPathRate(ctx context.Context, pc *promclient.Client, expr string) ([]pathRate, error) {
	samples, err := pc.Query(ctx, expr)
	if err != nil {
		return nil, err
	}
	out := make([]pathRate, 0, len(samples))
	for _, s := range samples {
		out = append(out, pathRate{Path: labelOr(s.Metric, "path", "?"), Value: s.Value})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Value > out[j].Value })
	return out, nil
}

func queryTopPathLatency(ctx context.Context, pc *promclient.Client, expr string) ([]pathLatency, error) {
	samples, err := pc.Query(ctx, expr)
	if err != nil {
		return nil, err
	}
	out := make([]pathLatency, 0, len(samples))
	for _, s := range samples {
		out = append(out, pathLatency{Path: labelOr(s.Metric, "path", "?"), P99: s.Value})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].P99 > out[j].P99 })
	return out, nil
}

func labelOr(m map[string]string, key, fallback string) string {
	if v, ok := m[key]; ok && v != "" {
		return v
	}
	return fallback
}

func firstErr(emptyNote string, err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, promclient.ErrUnavailable) {
		return "prometheus unavailable"
	}
	if err.Error() == "empty result" {
		return emptyNote
	}
	return err.Error()
}

func statusForRatioSLO(actual, target float64) string {
	if target <= 0 {
		return "unknown"
	}
	if actual >= target {
		return "ok"
	}
	gap := target - actual
	budget := 1 - target
	if budget > 0 && gap/budget < 0.1 {
		return "warning"
	}
	return "breached"
}

func statusForLatencySLO(actual, target float64) string {
	if target <= 0 {
		return "unknown"
	}
	if actual <= target {
		return "ok"
	}
	if actual <= target*1.1 {
		return "warning"
	}
	return "breached"
}

func statusForDurabilitySLO(actual, target float64) string {
	if actual <= target {
		return "ok"
	}
	if actual <= target+1 {
		return "warning"
	}
	return "breached"
}

func statusEmoji(s string) string {
	switch s {
	case "ok":
		return "✅"
	case "warning":
		return "⚠️"
	case "breached":
		return "🔥"
	default:
		return "—"
	}
}

// fetchBurnRateAlerts queries Alertmanager v2 /api/v2/alerts and
// filters down to alerts carrying a `slo` label (the burn-rate +
// SLO-anchored single-window alerts shipped by US-002/US-003). The
// "count over window" semantic is "currently firing during the report
// run" — Alertmanager does not expose historical firing counts; for
// historical roll-ups query the ALERTS metric in Prometheus directly.
func fetchBurnRateAlerts(ctx context.Context, hc *http.Client, baseURL string) ([]alertEntry, error) {
	if hc == nil {
		hc = http.DefaultClient
	}
	u := strings.TrimRight(baseURL, "/") + "/api/v2/alerts?active=true"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("alertmanager: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("alertmanager status %d: %s", resp.StatusCode, body)
	}
	var raw []struct {
		Labels   map[string]string `json:"labels"`
		StartsAt time.Time         `json:"startsAt"`
		Status   struct {
			State string `json:"state"`
		} `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("alertmanager decode: %w", err)
	}
	out := make([]alertEntry, 0, len(raw))
	for _, a := range raw {
		if _, ok := a.Labels["slo"]; !ok {
			continue
		}
		name := a.Labels["alertname"]
		out = append(out, alertEntry{
			Alertname: name,
			Labels:    a.Labels,
			StartsAt:  a.StartsAt.UTC(),
			State:     a.Status.State,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Alertname < out[j].Alertname })
	return out, nil
}

func renderSLOMarkdown(w io.Writer, r *sloReport) error {
	var b strings.Builder
	fmt.Fprintf(&b, "# Strata SLO compliance — %s window\n\n", r.Window)
	fmt.Fprintf(&b, "_Generated %s against `%s`._\n\n", r.GeneratedAt.Format(time.RFC3339), r.PrometheusURL)

	fmt.Fprintln(&b, "## SLO status")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "| Status | SLO | Target | Actual | Note |")
	fmt.Fprintln(&b, "|--------|-----|-------:|-------:|------|")
	for _, s := range r.SLOs {
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n",
			statusEmoji(s.Status), s.Name,
			formatValue(s.Target, s.Unit), formatValue(s.Actual, s.Unit),
			defaultDash(s.Note))
	}
	fmt.Fprintln(&b)

	fmt.Fprintln(&b, "## Top 5 5xx paths")
	fmt.Fprintln(&b)
	if len(r.Top5xxPaths) == 0 {
		fmt.Fprintln(&b, "_No 5xx samples in window._")
	} else {
		fmt.Fprintln(&b, "| Path | Rate (req/s) |")
		fmt.Fprintln(&b, "|------|-------------:|")
		for _, p := range r.Top5xxPaths {
			fmt.Fprintf(&b, "| %s | %.4f |\n", p.Path, p.Value)
		}
	}
	fmt.Fprintln(&b)

	fmt.Fprintln(&b, "## Top 5 slow paths (p99 GET/PUT)")
	fmt.Fprintln(&b)
	if len(r.TopSlowPaths) == 0 {
		fmt.Fprintln(&b, "_No latency samples in window._")
	} else {
		fmt.Fprintln(&b, "| Path | p99 (s) |")
		fmt.Fprintln(&b, "|------|--------:|")
		for _, p := range r.TopSlowPaths {
			fmt.Fprintf(&b, "| %s | %.3f |\n", p.Path, p.P99)
		}
	}
	fmt.Fprintln(&b)

	if r.BurnRateError != "" {
		fmt.Fprintln(&b, "## Burn-rate alerts")
		fmt.Fprintln(&b)
		fmt.Fprintf(&b, "_Alertmanager error: %s_\n\n", r.BurnRateError)
	} else if r.BurnRateAlerts != nil {
		fmt.Fprintln(&b, "## Burn-rate alerts (currently active)")
		fmt.Fprintln(&b)
		if len(r.BurnRateAlerts) == 0 {
			fmt.Fprintln(&b, "_No SLO-anchored alerts firing._")
		} else {
			fmt.Fprintln(&b, "| Alertname | SLO | Severity | Started |")
			fmt.Fprintln(&b, "|-----------|-----|----------|---------|")
			for _, a := range r.BurnRateAlerts {
				fmt.Fprintf(&b, "| %s | %s | %s | %s |\n",
					a.Alertname,
					labelOr(a.Labels, "slo", "—"),
					labelOr(a.Labels, "severity", "—"),
					a.StartsAt.Format(time.RFC3339))
			}
		}
		fmt.Fprintln(&b)
	}

	_, err := io.WriteString(w, b.String())
	return err
}

func formatValue(v float64, unit string) string {
	switch unit {
	case "ratio":
		return fmt.Sprintf("%.4f", v)
	case "seconds":
		return fmt.Sprintf("%.3fs", v)
	default:
		return fmt.Sprintf("%.3f", v)
	}
}

func defaultDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
