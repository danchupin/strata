package adminapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/danchupin/strata/internal/meta"
)

func TestStorageMetaShape(t *testing.T) {
	s := newTestServer()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/storage/meta", nil)
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	var report meta.MetaHealthReport
	if err := json.NewDecoder(rr.Body).Decode(&report); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if report.Backend != "memory" {
		t.Errorf("backend: got %q want memory", report.Backend)
	}
	if len(report.Nodes) != 1 {
		t.Fatalf("nodes: got %d want 1", len(report.Nodes))
	}
	n := report.Nodes[0]
	if n.Address == "" || n.State == "" {
		t.Errorf("node missing fields: %+v", n)
	}
	if report.ReplicationFactor != 1 {
		t.Errorf("rf: got %d want 1", report.ReplicationFactor)
	}
}
