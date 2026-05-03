package adminapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/meta"
)

// seedBucketWithOwner creates a bucket with the given owner and pre-loads
// `numObjects` objects of `size` bytes each so size/object_count sorts have
// distinguishable values.
func seedBucketWithOwner(t *testing.T, store meta.Store, name, owner string, numObjects int, size int64) {
	t.Helper()
	ctx := context.Background()
	if _, err := store.CreateBucket(ctx, name, owner, "STANDARD"); err != nil {
		t.Fatalf("create bucket %s: %v", name, err)
	}
	b, err := store.GetBucket(ctx, name)
	if err != nil {
		t.Fatalf("get bucket %s: %v", name, err)
	}
	for i := 0; i < numObjects; i++ {
		if err := store.PutObject(ctx, &meta.Object{
			BucketID: b.ID,
			Key:      fmt.Sprintf("k-%03d", i),
			Size:     size,
			ETag:     "deadbeef",
			IsLatest: true,
			Manifest: &data.Manifest{},
		}, false); err != nil {
			t.Fatalf("put: %v", err)
		}
	}
}

func decodeBucketsList(t *testing.T, body []byte) BucketsListResponse {
	t.Helper()
	var got BucketsListResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v body=%s", err, string(body))
	}
	return got
}

func bucketsListGET(t *testing.T, s *Server, rawQuery string) (int, BucketsListResponse) {
	t.Helper()
	rr := httptest.NewRecorder()
	url := "/admin/v1/buckets"
	if rawQuery != "" {
		url += "?" + rawQuery
	}
	req := httptest.NewRequest(http.MethodGet, url, nil)
	s.routes().ServeHTTP(rr, req)
	return rr.Code, decodeBucketsList(t, rr.Body.Bytes())
}

func TestBucketsListDefaultSortByCreatedDesc(t *testing.T) {
	s := newTestServer()
	// Memory backend stamps CreatedAt on CreateBucket. Sleep ensures monotonic
	// timestamps regardless of clock resolution.
	seedBucketWithOwner(t, s.Meta, "alpha", "alice", 0, 0)
	time.Sleep(2 * time.Millisecond)
	seedBucketWithOwner(t, s.Meta, "beta", "bob", 0, 0)
	time.Sleep(2 * time.Millisecond)
	seedBucketWithOwner(t, s.Meta, "gamma", "alice", 0, 0)

	code, got := bucketsListGET(t, s, "")
	if code != http.StatusOK {
		t.Fatalf("status=%d", code)
	}
	if got.Total != 3 {
		t.Errorf("total=%d want 3", got.Total)
	}
	if len(got.Buckets) != 3 {
		t.Fatalf("len=%d", len(got.Buckets))
	}
	if got.Buckets[0].Name != "gamma" {
		t.Errorf("first=%s want gamma (newest first)", got.Buckets[0].Name)
	}
	if got.Buckets[2].Name != "alpha" {
		t.Errorf("last=%s want alpha (oldest)", got.Buckets[2].Name)
	}
	if got.Buckets[0].Region != "test-region" {
		t.Errorf("region=%q want test-region (echoed from Server.Region)", got.Buckets[0].Region)
	}
	if got.Buckets[0].Owner != "alice" {
		t.Errorf("owner=%q want alice", got.Buckets[0].Owner)
	}
}

func TestBucketsListSortByNameAsc(t *testing.T) {
	s := newTestServer()
	seedBucketWithOwner(t, s.Meta, "zeta", "x", 0, 0)
	seedBucketWithOwner(t, s.Meta, "alpha", "x", 0, 0)
	seedBucketWithOwner(t, s.Meta, "mu", "x", 0, 0)

	_, got := bucketsListGET(t, s, "sort=name")
	names := []string{got.Buckets[0].Name, got.Buckets[1].Name, got.Buckets[2].Name}
	want := []string{"alpha", "mu", "zeta"}
	if names[0] != want[0] || names[1] != want[1] || names[2] != want[2] {
		t.Errorf("names=%v want %v", names, want)
	}
}

func TestBucketsListSortByOwnerDesc(t *testing.T) {
	s := newTestServer()
	seedBucketWithOwner(t, s.Meta, "a1", "alice", 0, 0)
	seedBucketWithOwner(t, s.Meta, "a2", "alice", 0, 0)
	seedBucketWithOwner(t, s.Meta, "b1", "bob", 0, 0)

	_, got := bucketsListGET(t, s, "sort=owner&order=desc")
	if got.Buckets[0].Owner != "bob" {
		t.Errorf("first owner=%q want bob", got.Buckets[0].Owner)
	}
	if got.Buckets[1].Owner != "alice" || got.Buckets[2].Owner != "alice" {
		t.Errorf("trailing owners=%q,%q want alice/alice", got.Buckets[1].Owner, got.Buckets[2].Owner)
	}
	// Tie-break: alphabetical name within the same owner.
	if got.Buckets[1].Name != "a1" || got.Buckets[2].Name != "a2" {
		t.Errorf("alice tie-break=%s,%s want a1,a2", got.Buckets[1].Name, got.Buckets[2].Name)
	}
}

func TestBucketsListSortBySizeDesc(t *testing.T) {
	s := newTestServer()
	seedBucketWithOwner(t, s.Meta, "small", "x", 1, 100)
	seedBucketWithOwner(t, s.Meta, "big", "x", 1, 10_000)
	seedBucketWithOwner(t, s.Meta, "medium", "x", 1, 1_000)

	_, got := bucketsListGET(t, s, "sort=size")
	if got.Buckets[0].Name != "big" || got.Buckets[0].SizeBytes != 10_000 {
		t.Errorf("first=%+v want big/10000", got.Buckets[0])
	}
	if got.Buckets[2].Name != "small" || got.Buckets[2].SizeBytes != 100 {
		t.Errorf("last=%+v want small/100", got.Buckets[2])
	}
}

func TestBucketsListSortByObjectCountAsc(t *testing.T) {
	s := newTestServer()
	seedBucketWithOwner(t, s.Meta, "many", "x", 5, 1)
	seedBucketWithOwner(t, s.Meta, "few", "x", 1, 1)
	seedBucketWithOwner(t, s.Meta, "some", "x", 3, 1)

	_, got := bucketsListGET(t, s, "sort=object_count&order=asc")
	if got.Buckets[0].Name != "few" || got.Buckets[0].ObjectCount != 1 {
		t.Errorf("first=%+v want few/1", got.Buckets[0])
	}
	if got.Buckets[2].Name != "many" || got.Buckets[2].ObjectCount != 5 {
		t.Errorf("last=%+v want many/5", got.Buckets[2])
	}
}

func TestBucketsListQueryFiltersCaseInsensitive(t *testing.T) {
	s := newTestServer()
	seedBucketWithOwner(t, s.Meta, "logs-prod", "x", 0, 0)
	seedBucketWithOwner(t, s.Meta, "LOGS-staging", "x", 0, 0)
	seedBucketWithOwner(t, s.Meta, "media", "x", 0, 0)

	_, got := bucketsListGET(t, s, "query=LOG&sort=name")
	if got.Total != 2 {
		t.Errorf("total=%d want 2", got.Total)
	}
	if len(got.Buckets) != 2 {
		t.Fatalf("len=%d", len(got.Buckets))
	}
	if got.Buckets[0].Name != "LOGS-staging" || got.Buckets[1].Name != "logs-prod" {
		t.Errorf("got=%v want [LOGS-staging logs-prod]", []string{got.Buckets[0].Name, got.Buckets[1].Name})
	}
}

func TestBucketsListPagination(t *testing.T) {
	s := newTestServer()
	for i := 0; i < 7; i++ {
		seedBucketWithOwner(t, s.Meta, fmt.Sprintf("b-%02d", i), "x", 0, 0)
	}
	// page_size=3 + page=2 → rows 3..5
	_, got := bucketsListGET(t, s, "sort=name&page=2&page_size=3")
	if got.Total != 7 {
		t.Errorf("total=%d want 7", got.Total)
	}
	if len(got.Buckets) != 3 {
		t.Fatalf("page rows=%d want 3", len(got.Buckets))
	}
	want := []string{"b-03", "b-04", "b-05"}
	for i, w := range want {
		if got.Buckets[i].Name != w {
			t.Errorf("row %d = %s want %s", i, got.Buckets[i].Name, w)
		}
	}
	// Out-of-range page returns empty rows but preserves total.
	_, got = bucketsListGET(t, s, "sort=name&page=99&page_size=3")
	if got.Total != 7 {
		t.Errorf("total on overflow page=%d want 7", got.Total)
	}
	if len(got.Buckets) != 0 {
		t.Errorf("overflow rows=%d want 0", len(got.Buckets))
	}
}

func TestBucketsListPageSizeClamp(t *testing.T) {
	s := newTestServer()
	for i := 0; i < 3; i++ {
		seedBucketWithOwner(t, s.Meta, fmt.Sprintf("c-%d", i), "x", 0, 0)
	}
	// page_size=99999 clamps to 500 (max). All 3 rows fit on page 1.
	_, got := bucketsListGET(t, s, "page_size=99999")
	if len(got.Buckets) != 3 {
		t.Errorf("len=%d want 3 (clamp does not truncate when total < cap)", len(got.Buckets))
	}
}

func TestBucketsListBadSortReturns400(t *testing.T) {
	s := newTestServer()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/buckets?sort=bogus", nil)
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400", rr.Code)
	}
}

func TestBucketsListBadOrderReturns400(t *testing.T) {
	s := newTestServer()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/buckets?order=sideways", nil)
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400", rr.Code)
	}
}

func TestBucketsListEmptyStore(t *testing.T) {
	s := newTestServer()
	code, got := bucketsListGET(t, s, "")
	if code != http.StatusOK {
		t.Errorf("status=%d", code)
	}
	if got.Total != 0 || got.Buckets == nil || len(got.Buckets) != 0 {
		t.Errorf("got=%+v want empty list", got)
	}
}
