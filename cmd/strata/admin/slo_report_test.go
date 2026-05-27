package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/danchupin/strata/internal/promclient"
)

// promResp is the Prometheus instant-query response wrapper used to
// synthesize fake samples from the mock server. Mirrors `data.result`
// shape — value is [<unix-ts-float>, "<value-string>"].
type promResp struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string             `json:"resultType"`
		Result     []promInstantEntry `json:"result"`
	} `json:"data"`
}

type promInstantEntry struct {
	Metric map[string]string `json:"metric"`
	Value  [2]any            `json:"value"`
}

func newPromMock(t *testing.T, byQuery map[string]promResp) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query" {
			http.NotFound(w, r)
			return
		}
		expr, err := url.QueryUnescape(r.URL.Query().Get("query"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		resp, ok := byQuery[expr]
		if !ok {
			// Default: empty vector — promclient handles this gracefully.
			resp = promResp{Status: "success"}
			resp.Data.ResultType = "vector"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func makeVector(samples ...promInstantEntry) promResp {
	r := promResp{Status: "success"}
	r.Data.ResultType = "vector"
	r.Data.Result = samples
	return r
}

func sample(labels map[string]string, value string) promInstantEntry {
	return promInstantEntry{Metric: labels, Value: [2]any{float64(1_700_000_000), value}}
}

func TestBuildSLOReport_HealthyWindow(t *testing.T) {
	byQuery := map[string]promResp{
		"strata:slo_availability:target":                                                         makeVector(sample(nil, "0.999")),
		"strata:slo_latency_get_put_seconds:target":                                              makeVector(sample(nil, "0.5")),
		"strata:slo_durability_error_rate:target":                                                makeVector(sample(nil, "0")),
		"avg_over_time(strata:availability:ratio_rate5m[7d])":                                    makeVector(sample(nil, "0.9999")),
		"avg_over_time(strata:latency_get_put:p99_rate5m[7d])":                                   makeVector(sample(nil, "0.21")),
		"sum(increase(strata_gc_terminal_ack_total{reason!=\"enoent\",reason!=\"ok\"}[7d]))":     makeVector(sample(nil, "0")),
		"topk(5, sum by (path) (rate(strata_http_requests_total{code=~\"5..\"}[7d])))":           makeVector(sample(map[string]string{"path": "/bkt/key1"}, "0.05")),
		"topk(5, histogram_quantile(0.99, sum by (le, path) (rate(strata_http_request_duration_seconds_bucket[7d]))))": makeVector(sample(map[string]string{"path": "/bkt/slowkey"}, "0.42")),
	}
	srv := newPromMock(t, byQuery)
	pc := promclient.New(srv.URL)

	rep, err := buildSLOReport(context.Background(), pc, "", "7d", http.DefaultClient)
	if err != nil {
		t.Fatalf("buildSLOReport: %v", err)
	}
	if len(rep.SLOs) != 3 {
		t.Fatalf("expected 3 SLO rows, got %d", len(rep.SLOs))
	}
	for _, s := range rep.SLOs {
		if s.Status != "ok" {
			t.Fatalf("SLO %q expected ok, got %q (target=%g actual=%g)", s.Name, s.Status, s.Target, s.Actual)
		}
	}
	if len(rep.Top5xxPaths) != 1 || rep.Top5xxPaths[0].Path != "/bkt/key1" {
		t.Fatalf("top 5xx paths unexpected: %+v", rep.Top5xxPaths)
	}
	if len(rep.TopSlowPaths) != 1 || rep.TopSlowPaths[0].Path != "/bkt/slowkey" {
		t.Fatalf("top slow paths unexpected: %+v", rep.TopSlowPaths)
	}

	var buf bytes.Buffer
	if err := renderSLOMarkdown(&buf, rep); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	mustContain(t, out, "# Strata SLO compliance — 7d window")
	mustContain(t, out, "## SLO status")
	mustContain(t, out, "Availability (5xx-free request ratio)")
	mustContain(t, out, "Latency p99 GET/PUT (seconds)")
	mustContain(t, out, "Durability (non-OK terminal GC acks)")
	mustContain(t, out, "/bkt/key1")
	mustContain(t, out, "/bkt/slowkey")
	mustContain(t, out, "✅")
}

func TestBuildSLOReport_BreachedSLOs(t *testing.T) {
	byQuery := map[string]promResp{
		"strata:slo_availability:target":                                                     makeVector(sample(nil, "0.999")),
		"strata:slo_latency_get_put_seconds:target":                                          makeVector(sample(nil, "0.5")),
		"strata:slo_durability_error_rate:target":                                            makeVector(sample(nil, "0")),
		"avg_over_time(strata:availability:ratio_rate5m[30d])":                               makeVector(sample(nil, "0.95")),
		"avg_over_time(strata:latency_get_put:p99_rate5m[30d])":                              makeVector(sample(nil, "1.2")),
		"sum(increase(strata_gc_terminal_ack_total{reason!=\"enoent\",reason!=\"ok\"}[30d]))": makeVector(sample(nil, "42")),
	}
	srv := newPromMock(t, byQuery)
	pc := promclient.New(srv.URL)

	rep, err := buildSLOReport(context.Background(), pc, "", "30d", http.DefaultClient)
	if err != nil {
		t.Fatalf("buildSLOReport: %v", err)
	}
	for _, s := range rep.SLOs {
		if s.Status != "breached" {
			t.Fatalf("SLO %q expected breached, got %q", s.Name, s.Status)
		}
	}

	var buf bytes.Buffer
	_ = renderSLOMarkdown(&buf, rep)
	mustContain(t, buf.String(), "🔥")
}

func TestBuildSLOReport_AlertmanagerIntegration(t *testing.T) {
	prom := newPromMock(t, map[string]promResp{
		"strata:slo_availability:target":                       makeVector(sample(nil, "0.999")),
		"strata:slo_latency_get_put_seconds:target":            makeVector(sample(nil, "0.5")),
		"strata:slo_durability_error_rate:target":              makeVector(sample(nil, "0")),
		"avg_over_time(strata:availability:ratio_rate5m[7d])":  makeVector(sample(nil, "0.9999")),
		"avg_over_time(strata:latency_get_put:p99_rate5m[7d])": makeVector(sample(nil, "0.21")),
		"sum(increase(strata_gc_terminal_ack_total{reason!=\"enoent\",reason!=\"ok\"}[7d]))": makeVector(sample(nil, "0")),
	})

	am := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/alerts" {
			http.NotFound(w, r)
			return
		}
		body := `[
		  {"labels":{"alertname":"StrataAvailabilityBurnRate5m1h","slo":"availability","severity":"critical","burn_window":"5m_1h"},"startsAt":"2026-05-27T10:00:00Z","status":{"state":"active"}},
		  {"labels":{"alertname":"StrataWorkerPanic","severity":"critical"},"startsAt":"2026-05-27T11:00:00Z","status":{"state":"active"}}
		]`
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	}))
	t.Cleanup(am.Close)

	pc := promclient.New(prom.URL)
	rep, err := buildSLOReport(context.Background(), pc, am.URL, "7d", http.DefaultClient)
	if err != nil {
		t.Fatalf("buildSLOReport: %v", err)
	}
	if rep.BurnRateError != "" {
		t.Fatalf("unexpected alertmanager error: %s", rep.BurnRateError)
	}
	if len(rep.BurnRateAlerts) != 1 || rep.BurnRateAlerts[0].Alertname != "StrataAvailabilityBurnRate5m1h" {
		t.Fatalf("expected single SLO-labelled alert, got %+v", rep.BurnRateAlerts)
	}

	var buf bytes.Buffer
	if err := renderSLOMarkdown(&buf, rep); err != nil {
		t.Fatalf("render: %v", err)
	}
	mustContain(t, buf.String(), "## Burn-rate alerts (currently active)")
	mustContain(t, buf.String(), "StrataAvailabilityBurnRate5m1h")
}

func TestBuildSLOReport_EmptyVectorsNotePopulated(t *testing.T) {
	srv := newPromMock(t, map[string]promResp{
		"strata:slo_availability:target":            makeVector(sample(nil, "0.999")),
		"strata:slo_latency_get_put_seconds:target": makeVector(sample(nil, "0.5")),
		"strata:slo_durability_error_rate:target":   makeVector(sample(nil, "0")),
	})
	pc := promclient.New(srv.URL)
	rep, err := buildSLOReport(context.Background(), pc, "", "7d", http.DefaultClient)
	if err != nil {
		t.Fatalf("buildSLOReport: %v", err)
	}
	for _, s := range rep.SLOs {
		if s.Note == "" {
			t.Fatalf("SLO %q expected note for missing samples, got empty", s.Name)
		}
	}
}

func TestSLOReport_AppRunMarkdown(t *testing.T) {
	srv := newPromMock(t, map[string]promResp{
		"strata:slo_availability:target":                                                     makeVector(sample(nil, "0.999")),
		"strata:slo_latency_get_put_seconds:target":                                          makeVector(sample(nil, "0.5")),
		"strata:slo_durability_error_rate:target":                                            makeVector(sample(nil, "0")),
		"avg_over_time(strata:availability:ratio_rate5m[7d])":                                makeVector(sample(nil, "0.9999")),
		"avg_over_time(strata:latency_get_put:p99_rate5m[7d])":                               makeVector(sample(nil, "0.21")),
		"sum(increase(strata_gc_terminal_ack_total{reason!=\"enoent\",reason!=\"ok\"}[7d]))": makeVector(sample(nil, "0")),
	})
	var stdout, stderr bytes.Buffer
	a := newApp(&stdout, &stderr, []string{
		"slo-report",
		"--prometheus-url", srv.URL,
		"--window", "7d",
	})
	if err := a.run(context.Background()); err != nil {
		t.Fatalf("run: %v stderr=%s", err, stderr.String())
	}
	mustContain(t, stdout.String(), "# Strata SLO compliance — 7d window")
	mustContain(t, stdout.String(), "✅")
}

func TestSLOReport_AppRunJSON(t *testing.T) {
	srv := newPromMock(t, map[string]promResp{
		"strata:slo_availability:target":            makeVector(sample(nil, "0.999")),
		"strata:slo_latency_get_put_seconds:target": makeVector(sample(nil, "0.5")),
		"strata:slo_durability_error_rate:target":   makeVector(sample(nil, "0")),
	})
	var stdout, stderr bytes.Buffer
	a := newApp(&stdout, &stderr, []string{
		"slo-report",
		"--prometheus-url", srv.URL,
		"--window", "7d",
		"--format", "json",
	})
	if err := a.run(context.Background()); err != nil {
		t.Fatalf("run: %v stderr=%s", err, stderr.String())
	}
	var rep sloReport
	if err := json.Unmarshal(stdout.Bytes(), &rep); err != nil {
		t.Fatalf("decode JSON: %v output=%s", err, stdout.String())
	}
	if rep.Window != "7d" || len(rep.SLOs) != 3 {
		t.Fatalf("unexpected report: %+v", rep)
	}
}

func TestSLOReport_RejectsInvalidWindow(t *testing.T) {
	var stdout, stderr bytes.Buffer
	a := newApp(&stdout, &stderr, []string{
		"slo-report",
		"--window", "12h",
	})
	if err := a.run(context.Background()); err == nil {
		t.Fatalf("expected --window validation error")
	}
}

func mustContain(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Fatalf("expected output to contain %q, got:\n%s", needle, haystack)
	}
}
