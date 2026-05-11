package adminapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/meta"
)

// refStubBackend is the minimal data.Backend + data.ClusterReferenceChecker
// stub used by TestDeleteClusterReferenced to exercise the 409 path. The
// handler only type-asserts the checker surface; the data.Backend surface
// stays unreachable but must exist so s.Data can be assigned.
type refStubBackend struct {
	refs []string
}

func (b *refStubBackend) PutChunks(context.Context, io.Reader, string) (*data.Manifest, error) {
	return nil, nil
}
func (b *refStubBackend) GetChunks(context.Context, *data.Manifest, int64, int64) (io.ReadCloser, error) {
	return nil, nil
}
func (b *refStubBackend) Delete(context.Context, *data.Manifest) error { return nil }
func (b *refStubBackend) Close(context.Context) error                  { return nil }
func (b *refStubBackend) ClassesUsingCluster(string) []string {
	out := append([]string(nil), b.refs...)
	sort.Strings(out)
	return out
}

func TestListClustersEmpty(t *testing.T) {
	s := newTestServer()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/storage/clusters", nil)
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	var got ClustersListResponse
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Clusters == nil {
		t.Error("clusters field nil; want empty slice")
	}
	if len(got.Clusters) != 0 {
		t.Errorf("clusters: %d want 0", len(got.Clusters))
	}
}

func TestCreateClusterHappyPath(t *testing.T) {
	s := newTestServer()
	body := []byte(`{"id":"cold-eu","backend":"rados","spec":{"config_file":"/etc/ceph/cold.conf","user":"admin"}}`)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/v1/storage/clusters", bytes.NewReader(body))
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status: got %d want 201 (body=%s)", rr.Code, rr.Body.String())
	}
	var got ClusterEntryResponse
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ID != "cold-eu" || got.Backend != "rados" {
		t.Errorf("entry: %+v", got)
	}
	if got.Version != 1 {
		t.Errorf("version: %d want 1", got.Version)
	}
	if got.CreatedAt == 0 || got.UpdatedAt == 0 {
		t.Errorf("timestamps: %+v", got)
	}

	// Round-trip via GET.
	rr2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/admin/v1/storage/clusters", nil)
	s.routes().ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusOK {
		t.Fatalf("list status: %d", rr2.Code)
	}
	var list ClustersListResponse
	if err := json.NewDecoder(rr2.Body).Decode(&list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list.Clusters) != 1 || list.Clusters[0].ID != "cold-eu" {
		t.Fatalf("list: %+v", list)
	}
}

func TestCreateClusterListSorted(t *testing.T) {
	s := newTestServer()
	for _, id := range []string{"zeta", "alpha", "mid-1"} {
		body := []byte(`{"id":"` + id + `","backend":"rados","spec":{"k":"v"}}`)
		rr := httptest.NewRecorder()
		s.routes().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/admin/v1/storage/clusters", bytes.NewReader(body)))
		if rr.Code != http.StatusCreated {
			t.Fatalf("insert %q: %d body=%s", id, rr.Code, rr.Body.String())
		}
	}
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/admin/v1/storage/clusters", nil))
	var got ClustersListResponse
	_ = json.NewDecoder(rr.Body).Decode(&got)
	ids := make([]string, 0, len(got.Clusters))
	for _, e := range got.Clusters {
		ids = append(ids, e.ID)
	}
	want := []string{"alpha", "mid-1", "zeta"}
	if !equalStrings(ids, want) {
		t.Errorf("order: %v want %v", ids, want)
	}
}

func TestCreateClusterValidation(t *testing.T) {
	s := newTestServer()
	cases := []struct {
		name string
		body string
		code string
	}{
		{"malformed JSON", `{"id":`, "BadRequest"},
		{"unknown backend", `{"id":"x","backend":"ceph","spec":{"k":"v"}}`, "InvalidArgument"},
		{"empty backend", `{"id":"x","backend":"","spec":{"k":"v"}}`, "InvalidArgument"},
		{"id uppercase", `{"id":"COLD","backend":"rados","spec":{"k":"v"}}`, "InvalidArgument"},
		{"id with slash", `{"id":"a/b","backend":"rados","spec":{"k":"v"}}`, "InvalidArgument"},
		{"id too long", `{"id":"` + repeat("a", 65) + `","backend":"rados","spec":{"k":"v"}}`, "InvalidArgument"},
		{"empty spec", `{"id":"a","backend":"rados"}`, "InvalidArgument"},
		{"null spec", `{"id":"a","backend":"rados","spec":null}`, "InvalidArgument"},
		{"empty object spec", `{"id":"a","backend":"rados","spec":{}}`, "InvalidArgument"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/admin/v1/storage/clusters",
				bytes.NewReader([]byte(tc.body)))
			s.routes().ServeHTTP(rr, req)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status: got %d want 400 (body=%s)", rr.Code, rr.Body.String())
			}
			var got errorResponse
			_ = json.NewDecoder(rr.Body).Decode(&got)
			if got.Code != tc.code {
				t.Errorf("code: got %q want %q", got.Code, tc.code)
			}
		})
	}
}

func TestCreateClusterConflict(t *testing.T) {
	s := newTestServer()
	body := []byte(`{"id":"dup","backend":"rados","spec":{"k":"v"}}`)
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/admin/v1/storage/clusters", bytes.NewReader(body)))
	if rr.Code != http.StatusCreated {
		t.Fatalf("first insert: %d body=%s", rr.Code, rr.Body.String())
	}
	rr = httptest.NewRecorder()
	s.routes().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/admin/v1/storage/clusters", bytes.NewReader(body)))
	if rr.Code != http.StatusConflict {
		t.Fatalf("second insert: got %d want 409 (body=%s)", rr.Code, rr.Body.String())
	}
	var got errorResponse
	_ = json.NewDecoder(rr.Body).Decode(&got)
	if got.Code != "ClusterAlreadyExists" {
		t.Errorf("code: got %q want ClusterAlreadyExists", got.Code)
	}
}

func TestDeleteClusterRoundTrip(t *testing.T) {
	s := newTestServer()
	if err := s.Meta.PutCluster(context.Background(), &meta.ClusterRegistryEntry{
		ID:      "to-drop",
		Backend: "rados",
		Spec:    []byte(`{"k":"v"}`),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, httptest.NewRequest(http.MethodDelete, "/admin/v1/storage/clusters/to-drop", nil))
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status: got %d want 204 (body=%s)", rr.Code, rr.Body.String())
	}
	if _, err := s.Meta.GetCluster(context.Background(), "to-drop"); err == nil {
		t.Error("cluster still present after delete")
	}
}

func TestDeleteClusterNotFound(t *testing.T) {
	s := newTestServer()
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, httptest.NewRequest(http.MethodDelete, "/admin/v1/storage/clusters/missing", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status: got %d want 404", rr.Code)
	}
	var got errorResponse
	_ = json.NewDecoder(rr.Body).Decode(&got)
	if got.Code != "NoSuchCluster" {
		t.Errorf("code: got %q", got.Code)
	}
}

func TestDeleteClusterReferenced(t *testing.T) {
	s := newTestServer()
	if err := s.Meta.PutCluster(context.Background(), &meta.ClusterRegistryEntry{
		ID:      "cold-eu",
		Backend: "rados",
		Spec:    []byte(`{"k":"v"}`),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Swap Data for a backend that reports two storage classes using
	// the cluster.
	s.Data = &refStubBackend{refs: []string{"COLD", "STANDARD"}}

	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, httptest.NewRequest(http.MethodDelete, "/admin/v1/storage/clusters/cold-eu", nil))
	if rr.Code != http.StatusConflict {
		t.Fatalf("status: got %d want 409 (body=%s)", rr.Code, rr.Body.String())
	}
	var got ClusterReferencedResponse
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Code != "ClusterReferenced" {
		t.Errorf("code: %q", got.Code)
	}
	sort.Strings(got.ReferencedBy)
	want := []string{"COLD", "STANDARD"}
	if !equalStrings(got.ReferencedBy, want) {
		t.Errorf("referenced_by: %v want %v", got.ReferencedBy, want)
	}
	// Underlying registry row must NOT have been deleted.
	if _, err := s.Meta.GetCluster(context.Background(), "cold-eu"); err != nil {
		t.Errorf("registry row was deleted despite 409: %v", err)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func repeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for range n {
		out = append(out, s...)
	}
	return string(out)
}
