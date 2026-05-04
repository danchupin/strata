package adminapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/meta"
)

// putObject is a small helper that pushes an object into a previously-created
// bucket without going through s3api. Tests use it to seed the read-only
// browser with predictable shapes.
func putObject(t *testing.T, s *Server, bucket, key string, size int64, etag, storageClass string) {
	t.Helper()
	ctx := context.Background()
	b, err := s.Meta.GetBucket(ctx, bucket)
	if err != nil {
		t.Fatalf("get bucket %s: %v", bucket, err)
	}
	if err := s.Meta.PutObject(ctx, &meta.Object{
		BucketID:     b.ID,
		Key:          key,
		Size:         size,
		ETag:         etag,
		StorageClass: storageClass,
		IsLatest:     true,
		Manifest:     &data.Manifest{},
	}, false); err != nil {
		t.Fatalf("put %s/%s: %v", bucket, key, err)
	}
}

func decodeBucketDetail(t *testing.T, body []byte) BucketDetail {
	t.Helper()
	var got BucketDetail
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v body=%s", err, string(body))
	}
	return got
}

func bucketDetailGET(t *testing.T, s *Server, bucket string) (int, BucketDetail) {
	t.Helper()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/buckets/"+bucket, nil)
	s.routes().ServeHTTP(rr, req)
	return rr.Code, decodeBucketDetail(t, rr.Body.Bytes())
}

func decodeObjectsList(t *testing.T, body []byte) ObjectsListResponse {
	t.Helper()
	var got ObjectsListResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v body=%s", err, string(body))
	}
	return got
}

func objectsListGET(t *testing.T, s *Server, bucket, rawQuery string) (int, ObjectsListResponse) {
	t.Helper()
	url := "/admin/v1/buckets/" + bucket + "/objects"
	if rawQuery != "" {
		url += "?" + rawQuery
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, url, nil)
	s.routes().ServeHTTP(rr, req)
	return rr.Code, decodeObjectsList(t, rr.Body.Bytes())
}

func TestBucketGetReturns404OnMissing(t *testing.T) {
	s := newTestServer()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/buckets/missing", nil)
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d want 404", rr.Code)
	}
	var er errorResponse
	_ = json.NewDecoder(rr.Body).Decode(&er)
	if er.Code != "NoSuchBucket" {
		t.Errorf("code=%q want NoSuchBucket", er.Code)
	}
}

func TestBucketGetReturnsDetail(t *testing.T) {
	s := newTestServer()
	seedBucketWithOwner(t, s.Meta, "data-lake", "alice", 0, 0)
	putObject(t, s, "data-lake", "a.txt", 100, "etag-a", "STANDARD")
	putObject(t, s, "data-lake", "b.txt", 250, "etag-b", "STANDARD")

	code, got := bucketDetailGET(t, s, "data-lake")
	if code != http.StatusOK {
		t.Fatalf("status=%d", code)
	}
	if got.Name != "data-lake" {
		t.Errorf("name=%q", got.Name)
	}
	if got.Owner != "alice" {
		t.Errorf("owner=%q", got.Owner)
	}
	if got.Region != "test-region" {
		t.Errorf("region=%q want test-region (echoed from Server.Region)", got.Region)
	}
	if got.Versioning != "Off" {
		t.Errorf("versioning=%q want Off (default Disabled→Off)", got.Versioning)
	}
	if got.ObjectLock {
		t.Errorf("object_lock=true; Phase 1 always reports false")
	}
	if got.SizeBytes != 350 {
		t.Errorf("size_bytes=%d want 350", got.SizeBytes)
	}
	if got.ObjectCount != 2 {
		t.Errorf("object_count=%d want 2", got.ObjectCount)
	}
	if got.CreatedAt <= 0 {
		t.Errorf("created_at=%d want >0", got.CreatedAt)
	}
	if got.ShardCount != 64 {
		t.Errorf("shard_count=%d want 64 (memory store default)", got.ShardCount)
	}
}

func TestBucketGetReportsVersioning(t *testing.T) {
	s := newTestServer()
	seedBucketWithOwner(t, s.Meta, "vbucket", "alice", 0, 0)
	if err := s.Meta.SetBucketVersioning(context.Background(), "vbucket", meta.VersioningEnabled); err != nil {
		t.Fatalf("set versioning: %v", err)
	}
	_, got := bucketDetailGET(t, s, "vbucket")
	if got.Versioning != "Enabled" {
		t.Errorf("versioning=%q want Enabled", got.Versioning)
	}
	if err := s.Meta.SetBucketVersioning(context.Background(), "vbucket", meta.VersioningSuspended); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	_, got = bucketDetailGET(t, s, "vbucket")
	if got.Versioning != "Suspended" {
		t.Errorf("versioning=%q want Suspended", got.Versioning)
	}
}

func TestObjectsListReturnsObjectsAndPrefixes(t *testing.T) {
	s := newTestServer()
	seedBucketWithOwner(t, s.Meta, "browse", "x", 0, 0)
	putObject(t, s, "browse", "README.md", 12, "etag-r", "STANDARD")
	putObject(t, s, "browse", "logs/2026/01.txt", 40, "etag-1", "STANDARD")
	putObject(t, s, "browse", "logs/2026/02.txt", 50, "etag-2", "STANDARD")
	putObject(t, s, "browse", "media/photo.jpg", 1024, "etag-p", "GLACIER")

	// Default delimiter '/' surfaces folders as common_prefixes at the root.
	code, got := objectsListGET(t, s, "browse", "")
	if code != http.StatusOK {
		t.Fatalf("status=%d", code)
	}
	if len(got.Objects) != 1 || got.Objects[0].Key != "README.md" {
		t.Errorf("root objects=%+v want [README.md]", got.Objects)
	}
	if got.Objects[0].Size != 12 || got.Objects[0].StorageClass != "STANDARD" {
		t.Errorf("README row=%+v", got.Objects[0])
	}
	wantPrefixes := map[string]bool{"logs/": true, "media/": true}
	if len(got.CommonPrefixes) != 2 {
		t.Fatalf("common_prefixes=%v want 2", got.CommonPrefixes)
	}
	for _, p := range got.CommonPrefixes {
		if !wantPrefixes[p] {
			t.Errorf("unexpected prefix %q", p)
		}
	}

	// Drilling in: prefix=logs/ should return both files, no common prefixes
	// when the day-level keys have no further '/' segments.
	_, got = objectsListGET(t, s, "browse", "prefix=logs%2F")
	if len(got.CommonPrefixes) != 1 {
		// logs/2026/ is one level deeper — surfaces as a prefix from this view.
		t.Errorf("common_prefixes=%v", got.CommonPrefixes)
	}
	if got.CommonPrefixes[0] != "logs/2026/" {
		t.Errorf("prefix=%q want logs/2026/", got.CommonPrefixes[0])
	}

	// Flat listing (delimiter=) returns every key under the prefix.
	_, got = objectsListGET(t, s, "browse", "prefix=logs%2F&delimiter=")
	if len(got.Objects) != 2 || len(got.CommonPrefixes) != 0 {
		t.Errorf("flat=%+v want 2 objects + 0 prefixes", got)
	}
}

func TestObjectsListPagination(t *testing.T) {
	s := newTestServer()
	seedBucketWithOwner(t, s.Meta, "many", "x", 0, 0)
	// 5 flat keys; page_size=2 forces two trip pages + a tail page.
	keys := []string{"a", "b", "c", "d", "e"}
	for _, k := range keys {
		putObject(t, s, "many", k, 1, "etag-"+k, "STANDARD")
	}
	_, got := objectsListGET(t, s, "many", "page_size=2")
	if len(got.Objects) != 2 {
		t.Fatalf("page1 objects=%d want 2", len(got.Objects))
	}
	if !got.IsTruncated || got.NextMarker == "" {
		t.Errorf("page1 truncated=%v marker=%q want truncated+nonempty marker", got.IsTruncated, got.NextMarker)
	}
	first := []string{got.Objects[0].Key, got.Objects[1].Key}
	if first[0] != "a" || first[1] != "b" {
		t.Errorf("page1 keys=%v want [a b]", first)
	}
	// Continuation token returns the next slice. The memory backend treats
	// NextMarker as exclusive — keys strictly after the marker are returned.
	// (Both Cassandra and TiKV walk the same way; this is the cursor contract
	// the admin API faithfully passes through.)
	_, got = objectsListGET(t, s, "many", "page_size=2&marker="+got.NextMarker)
	if len(got.Objects) == 0 {
		t.Fatalf("page2 empty; want continuation rows")
	}
	if got.Objects[0].Key <= "b" {
		t.Errorf("page2 first key=%q must be after marker b", got.Objects[0].Key)
	}
}

func TestObjectsListMissingBucketReturns404(t *testing.T) {
	s := newTestServer()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/buckets/missing/objects", nil)
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d want 404", rr.Code)
	}
	var er errorResponse
	_ = json.NewDecoder(rr.Body).Decode(&er)
	if er.Code != "NoSuchBucket" {
		t.Errorf("code=%q want NoSuchBucket", er.Code)
	}
}

func TestObjectsListEmptyBucketReturnsEmptyArrays(t *testing.T) {
	s := newTestServer()
	seedBucketWithOwner(t, s.Meta, "empty", "x", 0, 0)
	code, got := objectsListGET(t, s, "empty", "")
	if code != http.StatusOK {
		t.Fatalf("status=%d", code)
	}
	if got.Objects == nil || got.CommonPrefixes == nil {
		t.Error("nil arrays — must be empty slices for predictable JSON shape")
	}
	if len(got.Objects) != 0 || len(got.CommonPrefixes) != 0 {
		t.Errorf("got=%+v want empty", got)
	}
	if got.IsTruncated {
		t.Error("is_truncated true on empty result")
	}
}
