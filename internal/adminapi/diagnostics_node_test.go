package adminapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danchupin/strata/internal/heartbeat"
	"github.com/danchupin/strata/internal/promclient"
)

// nodeMatrixSeries is the same shape as matrixSeries (hot-buckets test) but
// keys on `instance` so the per-node Prom queries return realistic series.
func nodeMatrixSeries(instance string, points [][2]string) string {
	var b strings.Builder
	b.WriteString(`{"metric":{"instance":"` + instance + `"},"values":[`)
	for i, p := range points {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString("[" + p[0] + `,"` + p[1] + `"]`)
	}
	b.WriteString(`]}`)
	return b.String()
}

func seedHeartbeat(t *testing.T, s *Server, n heartbeat.Node) {
	t.Helper()
	if err := s.Heartbeat.WriteHeartbeat(context.Background(), n); err != nil {
		t.Fatalf("seed heartbeat: %v", err)
	}
}

func TestDiagnosticsNodeReturnsAllSparklines(t *testing.T) {
	var calls atomic.Int32
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		// Each query returns a one-point series so the response body is
		// deterministic per metric.
		expr := r.URL.Query().Get("query")
		var value string
		switch {
		case strings.HasPrefix(expr, "rate(process_cpu_seconds_total"):
			value = "0.5"
		case strings.HasPrefix(expr, "process_resident_memory_bytes"):
			value = "1048576"
		case strings.HasPrefix(expr, "process_open_fds"):
			value = "42"
		case strings.HasPrefix(expr, "go_goroutines"):
			value = "150"
		case strings.HasPrefix(expr, "go_gc_duration_seconds"):
			value = "0.001"
		default:
			t.Errorf("unexpected query=%q", expr)
		}
		body := fmt.Sprintf(matrixResponseBody, nodeMatrixSeries("127.0.0.1:9000",
			[][2]string{{"1700000000.0", value}}))
		_, _ = w.Write([]byte(body))
	}))
	defer prom.Close()

	s := newTestServer()
	s.Prom = promclient.New(prom.URL)
	seedHeartbeat(t, s, heartbeat.Node{
		ID:            "node-a",
		Address:       "127.0.0.1:9000",
		Version:       "test-sha",
		StartedAt:     time.Unix(1_700_000_000-3600, 0),
		Workers:       []string{"gc", "lifecycle"},
		LeaderFor:     []string{"gc-leader"},
		LastHeartbeat: time.Now().UTC(),
	})

	rr := httptest.NewRecorder()
	req := withAuditAuthCtx(httptest.NewRequest(http.MethodGet,
		"/admin/v1/diagnostics/node/node-a?range=15m", nil), "operator")
	s.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if got := calls.Load(); got != 5 {
		t.Errorf("prom calls=%d want 5", got)
	}
	var got NodeDrilldownResponse
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Node.ID != "node-a" {
		t.Errorf("node.id=%q want node-a", got.Node.ID)
	}
	if got.Node.Address != "127.0.0.1:9000" {
		t.Errorf("node.address=%q", got.Node.Address)
	}
	if len(got.Node.Workers) != 2 || got.Node.Workers[0] != "gc" {
		t.Errorf("node.workers=%v", got.Node.Workers)
	}
	if len(got.Node.LeaderFor) != 1 || got.Node.LeaderFor[0] != "gc-leader" {
		t.Errorf("node.leader_for=%v", got.Node.LeaderFor)
	}
	if len(got.CPU) != 1 || got.CPU[0].Value != 0.5 {
		t.Errorf("cpu=%+v", got.CPU)
	}
	if len(got.Mem) != 1 || got.Mem[0].Value != 1048576 {
		t.Errorf("mem=%+v", got.Mem)
	}
	if len(got.FDs) != 1 || got.FDs[0].Value != 42 {
		t.Errorf("fds=%+v", got.FDs)
	}
	if len(got.Goroutines) != 1 || got.Goroutines[0].Value != 150 {
		t.Errorf("goroutines=%+v", got.Goroutines)
	}
	if len(got.GCPause) != 1 || got.GCPause[0].Value != 0.001 {
		t.Errorf("gc_pause=%+v", got.GCPause)
	}
}

func TestDiagnosticsNodeUsesInstanceLabel(t *testing.T) {
	var seenQueries []string
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenQueries = append(seenQueries, r.URL.Query().Get("query"))
		body := fmt.Sprintf(matrixResponseBody, nodeMatrixSeries("10.0.0.5:9092",
			[][2]string{{"1700000000.0", "1"}}))
		_, _ = w.Write([]byte(body))
	}))
	defer prom.Close()

	s := newTestServer()
	s.Prom = promclient.New(prom.URL)
	seedHeartbeat(t, s, heartbeat.Node{
		ID:            "ceph-3",
		Address:       "10.0.0.5:9092",
		StartedAt:     time.Unix(1_700_000_000, 0),
		LastHeartbeat: time.Now().UTC(),
	})

	rr := httptest.NewRecorder()
	req := withAuditAuthCtx(httptest.NewRequest(http.MethodGet,
		"/admin/v1/diagnostics/node/ceph-3", nil), "operator")
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	for _, q := range seenQueries {
		if !strings.Contains(q, `instance="10.0.0.5:9092"`) {
			t.Errorf("query missing instance label: %q", q)
		}
	}
	if len(seenQueries) != 5 {
		t.Errorf("query count=%d want 5", len(seenQueries))
	}
}

func TestDiagnosticsNodeNotFound(t *testing.T) {
	s := newTestServer()
	rr := httptest.NewRecorder()
	req := withAuditAuthCtx(httptest.NewRequest(http.MethodGet,
		"/admin/v1/diagnostics/node/unknown", nil), "operator")
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d want 404", rr.Code)
	}
	var er errorResponse
	if err := json.NewDecoder(rr.Body).Decode(&er); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if er.Code != "NodeNotFound" {
		t.Errorf("code=%q want NodeNotFound", er.Code)
	}
}

func TestDiagnosticsNodePromUnavailable(t *testing.T) {
	s := newTestServer()
	seedHeartbeat(t, s, heartbeat.Node{
		ID:            "node-a",
		Address:       "127.0.0.1:9000",
		StartedAt:     time.Unix(1_700_000_000, 0),
		LastHeartbeat: time.Now().UTC(),
	})
	rr := httptest.NewRecorder()
	req := withAuditAuthCtx(httptest.NewRequest(http.MethodGet,
		"/admin/v1/diagnostics/node/node-a", nil), "operator")
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want 503", rr.Code)
	}
	var er errorResponse
	if err := json.NewDecoder(rr.Body).Decode(&er); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if er.Code != "MetricsUnavailable" {
		t.Errorf("code=%q want MetricsUnavailable", er.Code)
	}
}

func TestDiagnosticsNodeRejectsBadRange(t *testing.T) {
	s := newTestServer()
	seedHeartbeat(t, s, heartbeat.Node{
		ID:            "node-a",
		Address:       "127.0.0.1:9000",
		StartedAt:     time.Unix(1_700_000_000, 0),
		LastHeartbeat: time.Now().UTC(),
	})
	for _, q := range []string{"range=oops", "range=-1h", "range=0"} {
		t.Run(q, func(t *testing.T) {
			rr := httptest.NewRecorder()
			req := withAuditAuthCtx(httptest.NewRequest(http.MethodGet,
				"/admin/v1/diagnostics/node/node-a?"+q, nil), "operator")
			s.routes().ServeHTTP(rr, req)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
			}
		})
	}
}
