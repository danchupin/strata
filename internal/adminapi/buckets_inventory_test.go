package adminapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/danchupin/strata/internal/meta"
)

func seedInventoryBucket(t *testing.T, s *Server, name, owner string) *meta.Bucket {
	t.Helper()
	if _, err := s.Meta.CreateBucket(context.Background(), name, owner, "STANDARD"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	b, err := s.Meta.GetBucket(context.Background(), name)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	return b
}

func validInventoryBody(id string) InventoryConfigJSON {
	return InventoryConfigJSON{
		ID:        id,
		IsEnabled: true,
		Destination: InventoryDestJSON{
			Bucket: "arn:aws:s3:::dest",
			Format: "CSV",
			Prefix: "inv/",
		},
		Schedule:               InventoryScheduleJSON{Frequency: "Daily"},
		IncludedObjectVersions: "Current",
		Filter:                 &InventoryFilterJSON{Prefix: "logs/"},
		OptionalFields:         []string{"Size", "ETag", "StorageClass"},
	}
}

func TestBucketInventory_ListEmpty(t *testing.T) {
	s := newTestServer()
	seedInventoryBucket(t, s, "bkt", "alice")
	rr := putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/buckets/bkt/inventory", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got InventoryConfigsListJSON
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Configurations) != 0 {
		t.Errorf("configs=%v want empty", got.Configurations)
	}
}

func TestBucketInventory_ListBucketNotFound(t *testing.T) {
	s := newTestServer()
	rr := putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/buckets/missing/inventory", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d", rr.Code)
	}
	var er errorResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &er)
	if er.Code != "NoSuchBucket" {
		t.Errorf("code=%q", er.Code)
	}
}

func TestBucketInventory_PutHappyAndRoundTrip(t *testing.T) {
	s := newTestServer()
	seedInventoryBucket(t, s, "bkt", "alice")
	body := validInventoryBody("list1")
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/bkt/inventory/list1", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("put status=%d body=%s", rr.Code, rr.Body.String())
	}
	rr = putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/buckets/bkt/inventory", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list status=%d", rr.Code)
	}
	var got InventoryConfigsListJSON
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Configurations) != 1 {
		t.Fatalf("configs len=%d want 1", len(got.Configurations))
	}
	c := got.Configurations[0]
	if c.ID != "list1" || c.Destination.Bucket != "arn:aws:s3:::dest" || c.Destination.Format != "CSV" {
		t.Errorf("round-trip mismatch: %+v", c)
	}
	if c.Schedule.Frequency != "Daily" || c.IncludedObjectVersions != "Current" {
		t.Errorf("schedule/versions mismatch: %+v", c)
	}
	if c.Filter == nil || c.Filter.Prefix != "logs/" {
		t.Errorf("filter mismatch: %+v", c.Filter)
	}
	if len(c.OptionalFields) != 3 {
		t.Errorf("optional_fields=%v", c.OptionalFields)
	}
}

func TestBucketInventory_PutBucketNotFound(t *testing.T) {
	s := newTestServer()
	body := validInventoryBody("list1")
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/missing/inventory/list1", body)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestBucketInventory_PutIDMismatchRejected(t *testing.T) {
	s := newTestServer()
	seedInventoryBucket(t, s, "bkt", "alice")
	body := validInventoryBody("list1")
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/bkt/inventory/list2", body)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var er errorResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &er)
	if er.Code != "InvalidArgument" {
		t.Errorf("code=%q", er.Code)
	}
}

func TestBucketInventory_PutBadFormatRejected(t *testing.T) {
	s := newTestServer()
	seedInventoryBucket(t, s, "bkt", "alice")
	body := validInventoryBody("list1")
	body.Destination.Format = "JSON"
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/bkt/inventory/list1", body)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestBucketInventory_PutBadFrequencyRejected(t *testing.T) {
	s := newTestServer()
	seedInventoryBucket(t, s, "bkt", "alice")
	body := validInventoryBody("list1")
	body.Schedule.Frequency = "Yearly"
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/bkt/inventory/list1", body)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestBucketInventory_PutBadVersionsRejected(t *testing.T) {
	s := newTestServer()
	seedInventoryBucket(t, s, "bkt", "alice")
	body := validInventoryBody("list1")
	body.IncludedObjectVersions = "Past"
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/bkt/inventory/list1", body)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestBucketInventory_PutMissingDestBucketRejected(t *testing.T) {
	s := newTestServer()
	seedInventoryBucket(t, s, "bkt", "alice")
	body := validInventoryBody("list1")
	body.Destination.Bucket = ""
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/bkt/inventory/list1", body)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestBucketInventory_PutMalformedJSON(t *testing.T) {
	s := newTestServer()
	seedInventoryBucket(t, s, "bkt", "alice")
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/bkt/inventory/list1", "not json")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", rr.Code)
	}
}

func TestBucketInventory_PutBodyIDFallsBackToConfigID(t *testing.T) {
	s := newTestServer()
	seedInventoryBucket(t, s, "bkt", "alice")
	body := validInventoryBody("")
	rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/bkt/inventory/list1", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	rr = putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/buckets/bkt/inventory", nil)
	var got InventoryConfigsListJSON
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if len(got.Configurations) != 1 || got.Configurations[0].ID != "list1" {
		t.Errorf("configs=%+v", got.Configurations)
	}
}

func TestBucketInventory_DeleteHappy(t *testing.T) {
	s := newTestServer()
	seedInventoryBucket(t, s, "bkt", "alice")
	body := validInventoryBody("list1")
	if rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/bkt/inventory/list1", body); rr.Code != http.StatusOK {
		t.Fatalf("seed put: %d %s", rr.Code, rr.Body.String())
	}
	rr := putAdmin(t, s, "alice", http.MethodDelete, "/admin/v1/buckets/bkt/inventory/list1", nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete status=%d", rr.Code)
	}
	rr = putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/buckets/bkt/inventory", nil)
	var got InventoryConfigsListJSON
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if len(got.Configurations) != 0 {
		t.Errorf("post-delete list=%v", got.Configurations)
	}
}

func TestBucketInventory_DeleteIdempotent(t *testing.T) {
	s := newTestServer()
	seedInventoryBucket(t, s, "bkt", "alice")
	rr := putAdmin(t, s, "alice", http.MethodDelete, "/admin/v1/buckets/bkt/inventory/missing", nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status=%d", rr.Code)
	}
}

func TestBucketInventory_DeleteBucketNotFound(t *testing.T) {
	s := newTestServer()
	rr := putAdmin(t, s, "alice", http.MethodDelete, "/admin/v1/buckets/missing/inventory/x", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d", rr.Code)
	}
}

func TestBucketInventory_TwoConfigsListedSorted(t *testing.T) {
	s := newTestServer()
	seedInventoryBucket(t, s, "bkt", "alice")
	if rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/bkt/inventory/zeta",
		validInventoryBody("zeta")); rr.Code != http.StatusOK {
		t.Fatalf("put zeta: %d %s", rr.Code, rr.Body.String())
	}
	if rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/bkt/inventory/alpha",
		validInventoryBody("alpha")); rr.Code != http.StatusOK {
		t.Fatalf("put alpha: %d %s", rr.Code, rr.Body.String())
	}
	rr := putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/buckets/bkt/inventory", nil)
	var got InventoryConfigsListJSON
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if len(got.Configurations) != 2 || got.Configurations[0].ID != "alpha" || got.Configurations[1].ID != "zeta" {
		t.Errorf("expect sorted alpha,zeta; got %+v", got.Configurations)
	}
}

func TestBucketInventory_StoredAsXMLForS3APIConsumption(t *testing.T) {
	// The s3api consumer reads bytes verbatim — ensure the admin layer
	// stores the AWS XML shape, not our JSON.
	s := newTestServer()
	b := seedInventoryBucket(t, s, "bkt", "alice")
	if rr := putAdmin(t, s, "alice", http.MethodPut, "/admin/v1/buckets/bkt/inventory/list1",
		validInventoryBody("list1")); rr.Code != http.StatusOK {
		t.Fatalf("put: %d %s", rr.Code, rr.Body.String())
	}
	blob, err := s.Meta.GetBucketInventoryConfig(context.Background(), b.ID, "list1")
	if err != nil {
		t.Fatalf("get blob: %v", err)
	}
	if !strings.Contains(string(blob), "<InventoryConfiguration>") {
		t.Errorf("expected XML shape, got: %s", string(blob))
	}
	if !strings.Contains(string(blob), "<Id>list1</Id>") {
		t.Errorf("missing Id: %s", string(blob))
	}
	if !strings.Contains(string(blob), "<Frequency>Daily</Frequency>") {
		t.Errorf("missing Frequency: %s", string(blob))
	}
}
