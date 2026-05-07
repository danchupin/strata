package adminapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/danchupin/strata/internal/bucketstats"
	"github.com/danchupin/strata/internal/data"
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

func TestStorageDataShape(t *testing.T) {
	s := newTestServer()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/storage/data", nil)
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	var report data.DataHealthReport
	if err := json.NewDecoder(rr.Body).Decode(&report); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if report.Backend != "memory" {
		t.Errorf("backend: got %q want memory", report.Backend)
	}
	if len(report.Pools) != 1 {
		t.Fatalf("pools: got %d want 1", len(report.Pools))
	}
	p := report.Pools[0]
	if p.Name == "" || p.State == "" {
		t.Errorf("pool missing fields: %+v", p)
	}
	if p.NumReplicas != 1 {
		t.Errorf("num replicas: got %d want 1", p.NumReplicas)
	}
}

func TestStorageClassesEmptyWhenNoSnapshot(t *testing.T) {
	s := newTestServer()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/storage/classes", nil)
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	var got StorageClassesResponse
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Classes) != 0 {
		t.Errorf("classes: got %d entries, want 0", len(got.Classes))
	}
	if got.PoolsByClass == nil {
		t.Error("pools_by_class: want empty map, got nil")
	}
}

func TestStorageClassesReturnsSnapshot(t *testing.T) {
	s := newTestServer()
	snap := bucketstats.NewSnapshot(map[string]string{
		"STANDARD":   "data.standard",
		"GLACIER_IR": "data.glacier",
	})
	snap.SetClasses(map[string]bucketstats.ClassStat{
		"STANDARD":   {Bytes: 800, Objects: 4},
		"GLACIER_IR": {Bytes: 50, Objects: 1},
	})
	s.StorageClasses = snap

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/storage/classes", nil)
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}
	var got StorageClassesResponse
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Classes) != 2 {
		t.Fatalf("classes: %d want 2", len(got.Classes))
	}
	// Sorted bytes-desc → STANDARD first.
	if got.Classes[0].Class != "STANDARD" || got.Classes[0].Bytes != 800 || got.Classes[0].Objects != 4 {
		t.Errorf("classes[0]: %+v", got.Classes[0])
	}
	if got.Classes[1].Class != "GLACIER_IR" || got.Classes[1].Bytes != 50 || got.Classes[1].Objects != 1 {
		t.Errorf("classes[1]: %+v", got.Classes[1])
	}
	if got.PoolsByClass["STANDARD"] != "data.standard" {
		t.Errorf("pools STANDARD: %q", got.PoolsByClass["STANDARD"])
	}
	if got.PoolsByClass["GLACIER_IR"] != "data.glacier" {
		t.Errorf("pools GLACIER_IR: %q", got.PoolsByClass["GLACIER_IR"])
	}
}

func TestStorageDataReturns503WhenBackendNil(t *testing.T) {
	s := newTestServer()
	s.Data = nil
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/storage/data", nil)
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d want 503 (body=%s)", rr.Code, rr.Body.String())
	}
}
