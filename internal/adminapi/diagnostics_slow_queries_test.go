package adminapi

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/meta"
)

func TestDiagnosticsSlowQueriesFiltersByLatencyAndWindow(t *testing.T) {
	s := newTestServer()
	bucketID := uuid.New()
	now := time.Now().UTC()

	mk := func(latencyMS int, age time.Duration, action, resource string) *meta.AuditEvent {
		return &meta.AuditEvent{
			BucketID:    bucketID,
			Bucket:      "bkt",
			Time:        now.Add(-age),
			Principal:   "alice",
			Action:      action,
			Resource:    resource,
			Result:      "200",
			RequestID:   "req-" + action,
			SourceIP:    "10.0.0.5",
			TotalTimeMS: latencyMS,
		}
	}
	for _, e := range []*meta.AuditEvent{
		mk(1500, 1*time.Minute, "PutObject", "/bkt/img.jpg"),
		mk(250, 30*time.Second, "DeleteObject", "/bkt/old.txt"),
		mk(50, 30*time.Second, "PutObject", "/bkt/fast.bin"), // below 100ms cutoff
		mk(800, 2*time.Hour, "PutObject", "/bkt/aged.bin"),   // outside 15m window
	} {
		seedAuditEvent(t, s, e)
	}

	rr := httptest.NewRecorder()
	req := withAuditAuthCtx(httptest.NewRequest(http.MethodGet, "/admin/v1/diagnostics/slow-queries", nil), "operator")
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}

	var got slowQueriesResponse
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Rows) != 2 {
		t.Fatalf("rows=%d want 2 (%+v)", len(got.Rows), got.Rows)
	}
	if got.Rows[0].LatencyMS != 1500 || got.Rows[1].LatencyMS != 250 {
		t.Fatalf("rows not sorted by latency desc: %+v", got.Rows)
	}
	if got.Rows[0].Op != "PutObject" || got.Rows[0].ObjectKey != "img.jpg" || got.Rows[0].BucketID != bucketID.String() {
		t.Fatalf("row[0] mismatch: %+v", got.Rows[0])
	}
	if got.Rows[0].Status != 200 {
		t.Fatalf("row[0] status: got %d want 200", got.Rows[0].Status)
	}
	if got.NextPageToken != "" {
		t.Fatalf("unexpected next_page_token: %q", got.NextPageToken)
	}
}

func TestDiagnosticsSlowQueriesPaginationRoundTrip(t *testing.T) {
	s := newTestServer()
	bucketID := uuid.New()
	now := time.Now().UTC()
	for i := range 110 {
		seedAuditEvent(t, s, &meta.AuditEvent{
			BucketID:    bucketID,
			Bucket:      "bkt",
			Time:        now.Add(-time.Duration(i+1) * time.Second),
			Principal:   "alice",
			Action:      "PutObject",
			Resource:    "/bkt/k",
			Result:      "200",
			TotalTimeMS: 1000 + i,
		})
	}

	rr := httptest.NewRecorder()
	req := withAuditAuthCtx(httptest.NewRequest(http.MethodGet, "/admin/v1/diagnostics/slow-queries", nil), "operator")
	s.routes().ServeHTTP(rr, req)

	var page1 slowQueriesResponse
	if err := json.NewDecoder(rr.Body).Decode(&page1); err != nil {
		t.Fatalf("decode page1: %v", err)
	}
	if len(page1.Rows) != 100 || page1.NextPageToken == "" {
		t.Fatalf("page1 size=%d next=%q", len(page1.Rows), page1.NextPageToken)
	}
	// next_page_token is base64(EventID); sanity-check it decodes.
	if _, err := base64.RawURLEncoding.DecodeString(page1.NextPageToken); err != nil {
		t.Fatalf("page_token not raw-url-base64: %v", err)
	}

	rr2 := httptest.NewRecorder()
	req2 := withAuditAuthCtx(
		httptest.NewRequest(http.MethodGet, "/admin/v1/diagnostics/slow-queries?page_token="+page1.NextPageToken, nil),
		"operator",
	)
	s.routes().ServeHTTP(rr2, req2)
	var page2 slowQueriesResponse
	if err := json.NewDecoder(rr2.Body).Decode(&page2); err != nil {
		t.Fatalf("decode page2: %v", err)
	}
	if len(page2.Rows) != 10 {
		t.Fatalf("page2 size=%d want 10", len(page2.Rows))
	}
	if page2.NextPageToken != "" {
		t.Fatalf("page2 next_page_token=%q want empty", page2.NextPageToken)
	}
}

func TestDiagnosticsSlowQueriesValidatesArgs(t *testing.T) {
	s := newTestServer()
	cases := []struct {
		name string
		url  string
		code string
	}{
		{"bad_since", "/admin/v1/diagnostics/slow-queries?since=xyz", "InvalidArgument"},
		{"negative_min_ms", "/admin/v1/diagnostics/slow-queries?min_ms=-1", "InvalidArgument"},
		{"bad_page_token", "/admin/v1/diagnostics/slow-queries?page_token=" + strings.Repeat("!", 5), "InvalidArgument"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			req := withAuditAuthCtx(httptest.NewRequest(http.MethodGet, tc.url, nil), "operator")
			s.routes().ServeHTTP(rr, req)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status=%d want 400; body=%s", rr.Code, rr.Body.String())
			}
			var er errorResponse
			if err := json.NewDecoder(rr.Body).Decode(&er); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if er.Code != tc.code {
				t.Fatalf("code=%q want %q", er.Code, tc.code)
			}
		})
	}
}
