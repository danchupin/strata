package adminapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/danchupin/strata/internal/meta"
)

func TestUserQuota_GetNotConfigured(t *testing.T) {
	s := newTestServer()
	seedIAMUser(t, s, "alice")
	rr := putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/iam/users/alice/quota", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var er errorResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &er)
	if er.Code != "NoSuchUserQuota" {
		t.Errorf("code=%q want NoSuchUserQuota", er.Code)
	}
}

func TestUserQuota_GetUserNotFound(t *testing.T) {
	s := newTestServer()
	rr := putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/iam/users/missing/quota", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d", rr.Code)
	}
	var er errorResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &er)
	if er.Code != "NoSuchEntity" {
		t.Errorf("code=%q want NoSuchEntity", er.Code)
	}
}

func TestUserQuota_PutAndGetRoundTrip(t *testing.T) {
	s := newTestServer()
	seedIAMUser(t, s, "alice")
	body := UserQuotaJSON{MaxBuckets: 10, TotalMaxBytes: 1 << 40}
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/iam/users/alice/quota", body)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("put status=%d body=%s", rr.Code, rr.Body.String())
	}
	rr = putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/iam/users/alice/quota", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("get status=%d", rr.Code)
	}
	var got UserQuotaJSON
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got != body {
		t.Errorf("round-trip: got=%+v want=%+v", got, body)
	}
}

func TestUserQuota_PutNegativeFieldRejected(t *testing.T) {
	s := newTestServer()
	seedIAMUser(t, s, "alice")
	body := UserQuotaJSON{TotalMaxBytes: -1}
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/iam/users/alice/quota", body)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestUserQuota_PutMissingUserBlocks(t *testing.T) {
	s := newTestServer()
	body := UserQuotaJSON{MaxBuckets: 1}
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/iam/users/missing/quota", body)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestUserQuota_DeleteIdempotent(t *testing.T) {
	s := newTestServer()
	seedIAMUser(t, s, "alice")
	rr := putAdmin(t, s, "alice", http.MethodDelete, "/admin/v1/iam/users/alice/quota", nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("first status=%d", rr.Code)
	}
	rr = putAdmin(t, s, "alice", http.MethodDelete, "/admin/v1/iam/users/alice/quota", nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("second status=%d", rr.Code)
	}
}

func TestUserUsage_HappyPath(t *testing.T) {
	s := newTestServer()
	seedIAMUser(t, s, "alice")
	seedQuotaBucket(t, s, "bkt-a", "alice")
	seedQuotaBucket(t, s, "bkt-b", "alice")
	a, _ := s.Meta.GetBucket(context.Background(), "bkt-a")
	b, _ := s.Meta.GetBucket(context.Background(), "bkt-b")
	day := time.Date(2026, 5, 5, 0, 0, 0, 0, time.UTC)
	for _, agg := range []meta.UsageAggregate{
		{BucketID: a.ID, Bucket: "bkt-a", StorageClass: "STANDARD", Day: day, ByteSeconds: 100, ObjectCountAvg: 1, ObjectCountMax: 2},
		{BucketID: b.ID, Bucket: "bkt-b", StorageClass: "STANDARD", Day: day, ByteSeconds: 200, ObjectCountAvg: 3, ObjectCountMax: 4},
	} {
		if err := s.Meta.WriteUsageAggregate(context.Background(), agg); err != nil {
			t.Fatalf("write agg: %v", err)
		}
	}
	rr := putAdmin(t, s, "alice", http.MethodGet,
		"/admin/v1/iam/users/alice/usage?start=2026-05-05&end=2026-05-05", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got UserUsageResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Totals.ByteSeconds != 300 {
		t.Errorf("totals.byte_seconds=%d want 300", got.Totals.ByteSeconds)
	}
	if got.Totals.Objects != 6 {
		t.Errorf("totals.objects=%d want 6", got.Totals.Objects)
	}
	if len(got.Rows) == 0 {
		t.Fatalf("expected at least one row, got %+v", got)
	}
}

func TestUserUsage_UserNotFound(t *testing.T) {
	s := newTestServer()
	rr := putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/iam/users/missing/usage", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d", rr.Code)
	}
}
