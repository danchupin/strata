package adminapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/meta"
)

func seedQuotaBucket(t *testing.T, s *Server, name, owner string) {
	t.Helper()
	if _, err := s.Meta.CreateBucket(context.Background(), name, owner, "STANDARD"); err != nil {
		t.Fatalf("seed bucket: %v", err)
	}
}

// putAdminRaw mirrors putAdmin but accepts an already-shaped body reader so
// callers can submit deliberately-malformed JSON.
func putAdminRaw(t *testing.T, s *Server, owner, method, path string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, body)
	if owner != "" {
		ctx := auth.WithAuth(req.Context(), &auth.AuthInfo{AccessKey: owner, Owner: owner})
		req = req.WithContext(ctx)
	}
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, req)
	return rr
}

func TestBucketQuota_GetNotConfigured(t *testing.T) {
	s := newTestServer()
	seedQuotaBucket(t, s, "bkt", "alice")
	rr := putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/buckets/bkt/quota", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var er errorResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &er)
	if er.Code != "NoSuchBucketQuota" {
		t.Errorf("code=%q want NoSuchBucketQuota", er.Code)
	}
}

func TestBucketQuota_GetBucketNotFound(t *testing.T) {
	s := newTestServer()
	rr := putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/buckets/missing/quota", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d", rr.Code)
	}
	var er errorResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &er)
	if er.Code != "NoSuchBucket" {
		t.Errorf("code=%q want NoSuchBucket", er.Code)
	}
}

func TestBucketQuota_PutAndGetRoundTrip(t *testing.T) {
	s := newTestServer()
	seedQuotaBucket(t, s, "bkt", "alice")
	body := BucketQuotaJSON{MaxBytes: 1 << 30, MaxObjects: 1000, MaxBytesPerObject: 5 << 20}
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/bkt/quota", body)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("put status=%d body=%s", rr.Code, rr.Body.String())
	}
	rr = putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/buckets/bkt/quota", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("get status=%d", rr.Code)
	}
	var got BucketQuotaJSON
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got != body {
		t.Errorf("round-trip: got=%+v want=%+v", got, body)
	}
}

func TestBucketQuota_PutNegativeFieldRejected(t *testing.T) {
	s := newTestServer()
	seedQuotaBucket(t, s, "bkt", "alice")
	body := BucketQuotaJSON{MaxBytes: -1}
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/bkt/quota", body)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var er errorResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &er)
	if er.Code != "InvalidArgument" {
		t.Errorf("code=%q", er.Code)
	}
}

func TestBucketQuota_PutMalformedJSON(t *testing.T) {
	s := newTestServer()
	seedQuotaBucket(t, s, "bkt", "alice")
	rr := putAdminRaw(t, s, "alice", http.MethodPut, "/admin/v1/buckets/bkt/quota",
		strings.NewReader("not-json"))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestBucketQuota_DeleteIdempotent(t *testing.T) {
	s := newTestServer()
	seedQuotaBucket(t, s, "bkt", "alice")
	rr := putAdmin(t, s, "alice", http.MethodDelete, "/admin/v1/buckets/bkt/quota", nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("first delete status=%d", rr.Code)
	}
	rr = putAdmin(t, s, "alice", http.MethodDelete, "/admin/v1/buckets/bkt/quota", nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("second delete status=%d", rr.Code)
	}
}

func TestBucketUsage_HappyPath(t *testing.T) {
	s := newTestServer()
	seedQuotaBucket(t, s, "bkt", "alice")
	b, err := s.Meta.GetBucket(context.Background(), "bkt")
	if err != nil {
		t.Fatalf("getbucket: %v", err)
	}
	now := time.Now().UTC()
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -1)
	if err := s.Meta.WriteUsageAggregate(context.Background(), meta.UsageAggregate{
		BucketID:       b.ID,
		Bucket:         "bkt",
		StorageClass:   "STANDARD",
		Day:            day,
		ByteSeconds:    86400 * 1024,
		ObjectCountAvg: 5,
		ObjectCountMax: 5,
		ComputedAt:     now,
	}); err != nil {
		t.Fatalf("write agg: %v", err)
	}
	rr := putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/buckets/bkt/usage", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got BucketUsageResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Rows) != 1 {
		t.Fatalf("rows=%d want 1: %+v", len(got.Rows), got)
	}
	row := got.Rows[0]
	if row.Day != day.Format("2006-01-02") || row.StorageClass != "STANDARD" ||
		row.ByteSeconds != 86400*1024 || row.ObjectCountMax != 5 {
		t.Errorf("row=%+v", row)
	}
}

func TestBucketUsage_RangeWindow(t *testing.T) {
	s := newTestServer()
	seedQuotaBucket(t, s, "bkt", "alice")
	b, err := s.Meta.GetBucket(context.Background(), "bkt")
	if err != nil {
		t.Fatalf("getbucket: %v", err)
	}
	day := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	for i := range 3 {
		if err := s.Meta.WriteUsageAggregate(context.Background(), meta.UsageAggregate{
			BucketID:     b.ID,
			Bucket:       "bkt",
			StorageClass: "STANDARD",
			Day:          day.AddDate(0, 0, i),
			ByteSeconds:  int64(i + 1),
		}); err != nil {
			t.Fatalf("write agg: %v", err)
		}
	}
	rr := putAdmin(t, s, "alice", http.MethodGet,
		"/admin/v1/buckets/bkt/usage?start=2026-05-01&end=2026-05-02", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got BucketUsageResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Rows) != 2 {
		t.Fatalf("rows=%d want 2 (inclusive end): %+v", len(got.Rows), got)
	}
}

func TestBucketUsage_BadStart(t *testing.T) {
	s := newTestServer()
	seedQuotaBucket(t, s, "bkt", "alice")
	rr := putAdmin(t, s, "alice", http.MethodGet,
		"/admin/v1/buckets/bkt/usage?start=oops", nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestBucketUsage_BucketNotFound(t *testing.T) {
	s := newTestServer()
	rr := putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/buckets/missing/usage", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d", rr.Code)
	}
}
