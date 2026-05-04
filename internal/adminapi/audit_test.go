package adminapi

import (
	"context"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/meta"
)

// withAuditAuthCtx wraps a request context with an authenticated principal so
// audit-stamping middleware sees an Owner — adminapi tests bypass authMiddleware.
func withAuditAuthCtx(req *http.Request, owner string) *http.Request {
	ctx := auth.WithAuth(req.Context(), &auth.AuthInfo{AccessKey: owner, Owner: owner})
	return req.WithContext(ctx)
}

// seedAuditEvent inserts one audit row into the in-memory meta store. Returns
// the assigned EventID so tests can assert continuation token shape.
func seedAuditEvent(t *testing.T, s *Server, e *meta.AuditEvent) string {
	t.Helper()
	if err := s.Meta.EnqueueAudit(context.Background(), e, time.Hour); err != nil {
		t.Fatalf("enqueue audit: %v", err)
	}
	return e.EventID
}

func TestAuditListEmpty(t *testing.T) {
	s := newTestServer()
	rr := httptest.NewRecorder()
	req := withAuditAuthCtx(httptest.NewRequest(http.MethodGet, "/admin/v1/audit", nil), "operator")
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}
	var got auditListResponse
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Records == nil {
		t.Error("records: nil; want empty array")
	}
	if len(got.Records) != 0 {
		t.Errorf("records: got %d want 0", len(got.Records))
	}
	if got.NextPageToken != "" {
		t.Errorf("next_page_token: %q want empty", got.NextPageToken)
	}
}

func TestAuditListReturnsRowsNewestFirst(t *testing.T) {
	s := newTestServer()
	bucketID := uuid.New()
	t0 := time.Date(2026, 5, 4, 10, 0, 0, 0, time.UTC)
	seedAuditEvent(t, s, &meta.AuditEvent{
		BucketID: bucketID, Bucket: "alpha", Time: t0,
		Principal: "alice", Action: "PutObject", Resource: "alpha/key1",
		Result: "200", RequestID: "req-1", SourceIP: "10.0.0.1",
		UserAgent: "aws-cli/2.0",
	})
	seedAuditEvent(t, s, &meta.AuditEvent{
		BucketID: bucketID, Bucket: "alpha", Time: t0.Add(time.Minute),
		Principal: "bob", Action: "DeleteObject", Resource: "alpha/key1",
		Result: "204", RequestID: "req-2", SourceIP: "10.0.0.2",
	})

	rr := httptest.NewRecorder()
	req := withAuditAuthCtx(httptest.NewRequest(http.MethodGet, "/admin/v1/audit", nil), "operator")
	s.routes().ServeHTTP(rr, req)

	var got auditListResponse
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Records) != 2 {
		t.Fatalf("records: got %d want 2", len(got.Records))
	}
	if got.Records[0].Action != "DeleteObject" {
		t.Errorf("newest first: got %q want DeleteObject", got.Records[0].Action)
	}
	if got.Records[1].UserAgent != "aws-cli/2.0" {
		t.Errorf("user_agent: got %q want aws-cli/2.0", got.Records[1].UserAgent)
	}
	if got.Records[0].BucketID != bucketID.String() {
		t.Errorf("bucket_id: got %q", got.Records[0].BucketID)
	}
}

func TestAuditListFilterByPrincipal(t *testing.T) {
	s := newTestServer()
	bucketID := uuid.New()
	t0 := time.Now().UTC().Add(-time.Hour)
	seedAuditEvent(t, s, &meta.AuditEvent{
		BucketID: bucketID, Bucket: "b1", Time: t0,
		Principal: "alice", Action: "PutObject",
	})
	seedAuditEvent(t, s, &meta.AuditEvent{
		BucketID: bucketID, Bucket: "b1", Time: t0.Add(time.Second),
		Principal: "bob", Action: "PutObject",
	})

	rr := httptest.NewRecorder()
	req := withAuditAuthCtx(httptest.NewRequest(http.MethodGet, "/admin/v1/audit?principal=alice", nil), "operator")
	s.routes().ServeHTTP(rr, req)

	var got auditListResponse
	_ = json.NewDecoder(rr.Body).Decode(&got)
	if len(got.Records) != 1 || got.Records[0].Principal != "alice" {
		t.Fatalf("principal filter: %+v", got.Records)
	}
}

func TestAuditListFilterByActionMulti(t *testing.T) {
	s := newTestServer()
	bucketID := uuid.New()
	t0 := time.Now().UTC().Add(-time.Hour)
	for i, action := range []string{"PutObject", "DeleteObject", "ListBuckets", "PutBucketPolicy"} {
		seedAuditEvent(t, s, &meta.AuditEvent{
			BucketID: bucketID, Bucket: "b1", Time: t0.Add(time.Duration(i) * time.Second),
			Principal: "p", Action: action,
		})
	}

	rr := httptest.NewRecorder()
	req := withAuditAuthCtx(httptest.NewRequest(http.MethodGet,
		"/admin/v1/audit?action=PutObject,DeleteObject", nil), "operator")
	s.routes().ServeHTTP(rr, req)

	var got auditListResponse
	_ = json.NewDecoder(rr.Body).Decode(&got)
	if len(got.Records) != 2 {
		t.Fatalf("multi-action filter: got %d records: %+v", len(got.Records), got.Records)
	}
	for _, r := range got.Records {
		if r.Action != "PutObject" && r.Action != "DeleteObject" {
			t.Errorf("unexpected action %q", r.Action)
		}
	}
}

func TestAuditListFilterByBucket(t *testing.T) {
	s := newTestServer()
	if _, err := s.Meta.CreateBucket(context.Background(), "alpha", "owner", "STANDARD"); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	alpha, _ := s.Meta.GetBucket(context.Background(), "alpha")
	otherID := uuid.New()
	t0 := time.Now().UTC().Add(-time.Hour)
	seedAuditEvent(t, s, &meta.AuditEvent{
		BucketID: alpha.ID, Bucket: "alpha", Time: t0,
		Principal: "p", Action: "PutObject",
	})
	seedAuditEvent(t, s, &meta.AuditEvent{
		BucketID: otherID, Bucket: "beta", Time: t0,
		Principal: "p", Action: "PutObject",
	})

	rr := httptest.NewRecorder()
	req := withAuditAuthCtx(httptest.NewRequest(http.MethodGet, "/admin/v1/audit?bucket=alpha", nil), "operator")
	s.routes().ServeHTTP(rr, req)

	var got auditListResponse
	_ = json.NewDecoder(rr.Body).Decode(&got)
	if len(got.Records) != 1 || got.Records[0].Bucket != "alpha" {
		t.Fatalf("bucket filter: %+v", got.Records)
	}
}

func TestAuditListBucketNotFound(t *testing.T) {
	s := newTestServer()
	rr := httptest.NewRecorder()
	req := withAuditAuthCtx(httptest.NewRequest(http.MethodGet, "/admin/v1/audit?bucket=missing", nil), "operator")
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}
	var er errorResponse
	_ = json.NewDecoder(rr.Body).Decode(&er)
	if er.Code != "NoSuchBucket" {
		t.Errorf("code: %q want NoSuchBucket", er.Code)
	}
}

func TestAuditListInvalidSince(t *testing.T) {
	s := newTestServer()
	rr := httptest.NewRecorder()
	req := withAuditAuthCtx(httptest.NewRequest(http.MethodGet, "/admin/v1/audit?since=yesterday", nil), "operator")
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status: %d", rr.Code)
	}
	var er errorResponse
	_ = json.NewDecoder(rr.Body).Decode(&er)
	if er.Code != "InvalidArgument" {
		t.Errorf("code: %q", er.Code)
	}
}

func TestAuditListPagination(t *testing.T) {
	s := newTestServer()
	bucketID := uuid.New()
	t0 := time.Now().UTC().Add(-time.Hour)
	for i := 0; i < 5; i++ {
		seedAuditEvent(t, s, &meta.AuditEvent{
			BucketID: bucketID, Bucket: "b1", Time: t0.Add(time.Duration(i) * time.Second),
			Principal: "p", Action: "PutObject",
		})
	}

	rr1 := httptest.NewRecorder()
	req1 := withAuditAuthCtx(httptest.NewRequest(http.MethodGet, "/admin/v1/audit?limit=2", nil), "operator")
	s.routes().ServeHTTP(rr1, req1)
	var page1 auditListResponse
	_ = json.NewDecoder(rr1.Body).Decode(&page1)
	if len(page1.Records) != 2 {
		t.Fatalf("page1 size: %d want 2", len(page1.Records))
	}
	if page1.NextPageToken == "" {
		t.Fatal("expected next_page_token")
	}
	// Token must be valid base64.
	if _, err := base64.RawURLEncoding.DecodeString(page1.NextPageToken); err != nil {
		t.Errorf("next_page_token not base64: %v", err)
	}

	rr2 := httptest.NewRecorder()
	req2 := withAuditAuthCtx(httptest.NewRequest(http.MethodGet,
		"/admin/v1/audit?limit=2&page_token="+page1.NextPageToken, nil), "operator")
	s.routes().ServeHTTP(rr2, req2)
	var page2 auditListResponse
	_ = json.NewDecoder(rr2.Body).Decode(&page2)
	if len(page2.Records) != 2 {
		t.Fatalf("page2 size: %d want 2", len(page2.Records))
	}
	// page1 + page2 records must be distinct.
	for _, r1 := range page1.Records {
		for _, r2 := range page2.Records {
			if r1.EventID == r2.EventID {
				t.Errorf("event %q appeared on both pages", r1.EventID)
			}
		}
	}
}

func TestAuditListInvalidPageToken(t *testing.T) {
	s := newTestServer()
	rr := httptest.NewRecorder()
	req := withAuditAuthCtx(httptest.NewRequest(http.MethodGet, "/admin/v1/audit?page_token=!!!not-base64!!!", nil), "operator")
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status: %d", rr.Code)
	}
}

func TestAuditCSVHappy(t *testing.T) {
	s := newTestServer()
	bucketID := uuid.New()
	t0 := time.Now().UTC().Add(-time.Hour)
	seedAuditEvent(t, s, &meta.AuditEvent{
		BucketID: bucketID, Bucket: "alpha", Time: t0,
		Principal: "alice", Action: "PutObject", Resource: "alpha/key1",
		Result: "200", RequestID: "req-1", SourceIP: "10.0.0.1",
		UserAgent: "aws-cli/2.0",
	})
	seedAuditEvent(t, s, &meta.AuditEvent{
		BucketID: bucketID, Bucket: "alpha", Time: t0.Add(time.Second),
		Principal: "bob", Action: "DeleteObject",
	})

	rr := httptest.NewRecorder()
	req := withAuditAuthCtx(httptest.NewRequest(http.MethodGet, "/admin/v1/audit.csv", nil), "operator")
	s.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Errorf("content-type: %q want text/csv", ct)
	}
	if cd := rr.Header().Get("Content-Disposition"); !strings.Contains(cd, "audit.csv") {
		t.Errorf("content-disposition: %q", cd)
	}

	rows, err := csv.NewReader(strings.NewReader(rr.Body.String())).ReadAll()
	if err != nil {
		t.Fatalf("csv parse: %v", err)
	}
	if len(rows) < 3 { // header + 2 data
		t.Fatalf("csv rows: got %d want >=3", len(rows))
	}
	if rows[0][0] != "time" || rows[0][8] != "user_agent" {
		t.Errorf("csv header shape: %v", rows[0])
	}
	// Look for the aws-cli/2.0 user-agent in any row (ordering newest-first).
	found := false
	for _, r := range rows[1:] {
		if len(r) >= 9 && r[8] == "aws-cli/2.0" {
			found = true
		}
	}
	if !found {
		t.Errorf("user_agent column missing from CSV body: %v", rows)
	}
}

func TestAuditCSVActionFilter(t *testing.T) {
	s := newTestServer()
	bucketID := uuid.New()
	t0 := time.Now().UTC().Add(-time.Hour)
	for i, action := range []string{"PutObject", "DeleteObject", "ListBuckets"} {
		seedAuditEvent(t, s, &meta.AuditEvent{
			BucketID: bucketID, Bucket: "b1", Time: t0.Add(time.Duration(i) * time.Second),
			Principal: "p", Action: action,
		})
	}

	rr := httptest.NewRecorder()
	req := withAuditAuthCtx(httptest.NewRequest(http.MethodGet,
		"/admin/v1/audit.csv?action=PutObject", nil), "operator")
	s.routes().ServeHTTP(rr, req)

	rows, err := csv.NewReader(strings.NewReader(rr.Body.String())).ReadAll()
	if err != nil {
		t.Fatalf("csv parse: %v", err)
	}
	// Header row + exactly one matching data row.
	if len(rows) != 2 {
		t.Fatalf("csv rows after filter: got %d want 2; rows=%v", len(rows), rows)
	}
	if rows[1][3] != "PutObject" {
		t.Errorf("action column: %q want PutObject", rows[1][3])
	}
}
