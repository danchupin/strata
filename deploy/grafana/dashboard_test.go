package grafana_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

type panel struct {
	Type    string         `json:"type"`
	Title   string         `json:"title"`
	Targets []panelTarget  `json:"targets"`
	Panels  []panel        `json:"panels"`
	Options map[string]any `json:"options"`
}

type panelTarget struct {
	Expr string `json:"expr"`
}

type dashboard struct {
	Title         string  `json:"title"`
	UID           string  `json:"uid"`
	SchemaVersion int     `json:"schemaVersion"`
	Panels        []panel `json:"panels"`
}

func TestDashboardJSONValid(t *testing.T) {
	b, err := os.ReadFile("strata-dashboard.json")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var d dashboard
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if d.Title == "" || d.UID == "" {
		t.Fatalf("missing title/uid: %+v", d)
	}
	if d.SchemaVersion < 30 {
		t.Fatalf("schemaVersion too old: %d", d.SchemaVersion)
	}
	if len(d.Panels) == 0 {
		t.Fatalf("no panels")
	}
}

// TestDashboardHasRequiredPanels asserts every PRD-mandated panel/metric is
// referenced by at least one panel target. Keeps the dashboard honest as
// metrics evolve.
func TestDashboardHasRequiredPanels(t *testing.T) {
	b, err := os.ReadFile("strata-dashboard.json")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var d dashboard
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	allExprs := collectExprs(d.Panels)
	required := map[string]string{
		"http p50/p95/p99": "strata_http_request_duration_seconds_bucket",
		"cassandra latency by table": "strata_cassandra_query_duration_seconds_bucket",
		"rados latency by pool":      "strata_rados_op_duration_seconds_bucket",
		"gc queue depth":             "strata_gc_queue_depth",
		"multipart active":           "strata_multipart_active",
		"bucket bytes by class":      "strata_bucket_bytes",
		"lifecycle tick rate":        "strata_lifecycle_tick_total",
		"replication lag":            "strata_replication_lag_seconds_bucket",
		"replication queue depth":    "strata_replication_queue_depth",
		"notify delivery":            "strata_notify_delivery_total",
	}
	for label, metric := range required {
		if !anyContains(allExprs, metric) {
			t.Errorf("required panel missing: %s (metric %q not referenced)", label, metric)
		}
	}

	// HTTP latency panel must compute three quantiles.
	for _, q := range []string{"0.50", "0.95", "0.99"} {
		if !anyContains(allExprs, "histogram_quantile("+q+",") {
			t.Errorf("http latency quantile %s missing", q)
		}
	}
}

func collectExprs(panels []panel) []string {
	var out []string
	for _, p := range panels {
		for _, t := range p.Targets {
			if t.Expr != "" {
				out = append(out, t.Expr)
			}
		}
		out = append(out, collectExprs(p.Panels)...)
	}
	return out
}

func anyContains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}
