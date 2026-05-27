package grafana_test

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

type panel struct {
	ID      int            `json:"id"`
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

// dashboardSchemaVersionMin pins the Grafana 10 baseline asserted across every
// dashboard JSON shipped under deploy/grafana/. Bumping requires a Grafana
// upgrade verification.
const dashboardSchemaVersionMin = 39

// dashboardPaths returns every *.json dashboard shipped under deploy/grafana/
// (hero) and deploy/grafana/dashboards/ (specialized boards). Returned in
// stable sort order so failures cite a predictable path.
func dashboardPaths(t *testing.T) []string {
	t.Helper()
	var paths []string
	for _, dir := range []string{".", "dashboards"} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if dir == "dashboards" && os.IsNotExist(err) {
				continue
			}
			t.Fatalf("readdir %s: %v", dir, err)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			paths = append(paths, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(paths)
	if len(paths) == 0 {
		t.Fatalf("no dashboard JSON files found under deploy/grafana/")
	}
	return paths
}

func TestDashboardJSONValid(t *testing.T) {
	for _, path := range dashboardPaths(t) {
		t.Run(filepath.Base(path), func(t *testing.T) {
			b, err := os.ReadFile(path)
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
			if d.SchemaVersion < dashboardSchemaVersionMin {
				t.Fatalf("schemaVersion %d < %d (Grafana 10 baseline)", d.SchemaVersion, dashboardSchemaVersionMin)
			}
			if len(d.Panels) == 0 {
				t.Fatalf("no panels")
			}
		})
	}
}

// TestDashboardHasRequiredPanels asserts every PRD-mandated panel/metric is
// referenced by at least one hero-dashboard panel target. Keeps the dashboard
// honest as metrics evolve.
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
		"http p50/p95/p99":           "strata_http_request_duration_seconds_bucket",
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

// TestDashboardMetricsRegisteredInExporter is the cross-dashboard drift-lint
// extension (US-010): every `strata_<name>` metric referenced in any
// panel.targets[].expr (including nested panels) must be registered in
// internal/metrics/metrics.go. Histogram virtual-series suffixes _bucket /
// _count / _sum are stripped only when the stripped name is itself
// registered — a metric whose real name ends in _count (e.g.
// strata_rados_cluster_object_count) matches before strip. Non-strata
// metrics (process_*, go_*, prometheus_*, node_*) are out of scope by the
// regex itself.
func TestDashboardMetricsRegisteredInExporter(t *testing.T) {
	known := loadRegisteredMetrics(t)
	metricRE := regexp.MustCompile(`strata_[a-z_][a-z0-9_]*`)

	for _, path := range dashboardPaths(t) {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: read: %v", path, err)
		}
		var d dashboard
		if err := json.Unmarshal(b, &d); err != nil {
			t.Fatalf("%s: unmarshal: %v", path, err)
		}
		walkPanels(d.Panels, func(p panel) {
			for _, target := range p.Targets {
				if target.Expr == "" {
					continue
				}
				for _, match := range metricRE.FindAllString(target.Expr, -1) {
					if resolveRegisteredMetric(match, known) {
						continue
					}
					t.Errorf("%s: panel id=%d %q references unknown metric %q — register in internal/metrics/metrics.go or fix the dashboard expr",
						path, p.ID, p.Title, match)
				}
			}
		})
	}
}

// resolveRegisteredMetric returns true when name (or its histogram base form)
// is in the registered-metric set. Tries the full name first so a real metric
// ending in _count (e.g. strata_rados_cluster_object_count) is not mis-stripped
// to its histogram-base form.
func resolveRegisteredMetric(name string, known map[string]struct{}) bool {
	if _, ok := known[name]; ok {
		return true
	}
	for _, sfx := range []string{"_bucket", "_count", "_sum"} {
		if base, found := strings.CutSuffix(name, sfx); found {
			if _, ok := known[base]; ok {
				return true
			}
		}
	}
	return false
}

// loadRegisteredMetrics parses internal/metrics/*.go (non-test files) via
// go/ast and extracts every `Name: "strata_..."` string literal — the
// canonical metric-name source-of-truth used by prometheus.*Opts struct
// literals. Returns a set keyed by metric name.
func loadRegisteredMetrics(t *testing.T) map[string]struct{} {
	t.Helper()
	matches, err := filepath.Glob("../../internal/metrics/*.go")
	if err != nil {
		t.Fatalf("glob internal/metrics: %v", err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, p := range matches {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", p, err)
		}
		files = append(files, file)
	}
	if len(files) == 0 {
		t.Fatalf("no .go source files found under internal/metrics")
	}
	out := map[string]struct{}{}
	for _, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			kv, ok := n.(*ast.KeyValueExpr)
			if !ok {
				return true
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok || key.Name != "Name" {
				return true
			}
			lit, ok := kv.Value.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			name, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			if strings.HasPrefix(name, "strata_") {
				out[name] = struct{}{}
			}
			return true
		})
	}
	if len(out) == 0 {
		t.Fatalf("loadRegisteredMetrics found 0 metrics — parser regression?")
	}
	return out
}

func walkPanels(panels []panel, fn func(panel)) {
	for _, p := range panels {
		fn(p)
		walkPanels(p.Panels, fn)
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
