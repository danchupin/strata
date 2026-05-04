package adminapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/data"
	datamem "github.com/danchupin/strata/internal/data/memory"
	"github.com/danchupin/strata/internal/meta"
	metamem "github.com/danchupin/strata/internal/meta/memory"
	"github.com/danchupin/strata/internal/s3api"
)

// newObjectTestServer wires the admin Server with a real s3api handler so
// the DELETE proxy lands on a working object-delete pipeline. The seed
// bucket's owner matches the AccessKey/Owner stamped on every test request,
// so requireObjectAccess short-circuits via b.Owner == info.Owner.
func newObjectTestServer(t *testing.T) (*Server, *meta.Bucket) {
	t.Helper()
	store := metamem.New()
	dataBackend := datamem.New()
	cred := &auth.Credential{
		AccessKey: "AKIAOPS",
		Secret:    "secret-ops",
		Owner:     "AKIAOPS",
	}
	creds := auth.NewStaticStore(map[string]*auth.Credential{cred.AccessKey: cred})
	s := New(Config{
		Meta:        store,
		Creds:       creds,
		Region:      "us-east-1",
		MetaBackend: "memory",
		DataBackend: "memory",
		JWTSecret:   []byte("0123456789abcdef0123456789abcdef"),
	})
	s.S3Handler = s3api.New(dataBackend, store)
	if h, ok := s.S3Handler.(*s3api.Server); ok {
		h.Region = "us-east-1"
	}
	b, err := store.CreateBucket(context.Background(), "objbkt", "AKIAOPS", "STANDARD")
	if err != nil {
		t.Fatalf("seed bucket: %v", err)
	}
	return s, b
}

// objectAdminRequest dispatches a request through the route mux with an
// authenticated owner stamped onto the context.
func objectAdminRequest(t *testing.T, s *Server, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req = req.WithContext(auth.WithAuth(req.Context(), &auth.AuthInfo{
		AccessKey: "AKIAOPS", Owner: "AKIAOPS",
	}))
	req.Host = "strata.local:9000"
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, req)
	return rr
}

func putObjectWithMeta(t *testing.T, s *Server, b *meta.Bucket, key string, fn func(*meta.Object)) {
	t.Helper()
	o := &meta.Object{
		BucketID:     b.ID,
		Key:          key,
		Size:         42,
		ETag:         "etag-" + key,
		StorageClass: "STANDARD",
		IsLatest:     true,
		Mtime:        time.Now().UTC(),
		Manifest:     &data.Manifest{},
	}
	if fn != nil {
		fn(o)
	}
	if err := s.Meta.PutObject(context.Background(), o, false); err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
}

func TestObjectGet_HappyAndShape(t *testing.T) {
	s, b := newObjectTestServer(t)
	putObjectWithMeta(t, s, b, "doc.txt", func(o *meta.Object) {
		o.Size = 100
		o.ETag = "abc"
		o.ContentType = "text/plain"
		o.Tags = map[string]string{"env": "prod"}
		o.RetainMode = meta.LockModeGovernance
		o.RetainUntil = time.Now().Add(48 * time.Hour).UTC()
		o.LegalHold = true
	})
	rr := objectAdminRequest(t, s, http.MethodGet,
		"/admin/v1/buckets/objbkt/object?key=doc.txt", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got ObjectDetailResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Key != "doc.txt" || got.Size != 100 || got.ETag != "abc" {
		t.Errorf("base fields: %+v", got)
	}
	if got.Tags["env"] != "prod" {
		t.Errorf("tags=%+v want env=prod", got.Tags)
	}
	if got.RetainMode != meta.LockModeGovernance {
		t.Errorf("retain_mode=%q", got.RetainMode)
	}
	if got.RetainUntil <= 0 {
		t.Errorf("retain_until=%d want >0", got.RetainUntil)
	}
	if !got.LegalHold {
		t.Error("legal_hold=false want true")
	}
}

func TestObjectGet_NotFoundReturns404NoSuchKey(t *testing.T) {
	s, _ := newObjectTestServer(t)
	rr := objectAdminRequest(t, s, http.MethodGet,
		"/admin/v1/buckets/objbkt/object?key=missing.txt", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d", rr.Code)
	}
	var er errorResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &er)
	if er.Code != "NoSuchKey" {
		t.Errorf("code=%q want NoSuchKey", er.Code)
	}
}

func TestObjectGet_BucketNotFoundReturns404(t *testing.T) {
	s, _ := newObjectTestServer(t)
	rr := objectAdminRequest(t, s, http.MethodGet,
		"/admin/v1/buckets/missing/object?key=anything", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d", rr.Code)
	}
	var er errorResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &er)
	if er.Code != "NoSuchBucket" {
		t.Errorf("code=%q want NoSuchBucket", er.Code)
	}
}

func TestObjectTags_HappyAndRoundTrip(t *testing.T) {
	s, b := newObjectTestServer(t)
	putObjectWithMeta(t, s, b, "doc.txt", nil)

	rr := objectAdminRequest(t, s, http.MethodPut,
		"/admin/v1/buckets/objbkt/object-tags",
		SetObjectTagsRequest{Key: "doc.txt", Tags: map[string]string{"team": "data", "env": "prod"}})
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}

	tags, err := s.Meta.GetObjectTags(context.Background(), b.ID, "doc.txt", "")
	if err != nil {
		t.Fatalf("get tags: %v", err)
	}
	if tags["team"] != "data" || tags["env"] != "prod" {
		t.Errorf("stored tags=%+v want {team:data, env:prod}", tags)
	}

	// Empty map clears all tags.
	rr = objectAdminRequest(t, s, http.MethodPut,
		"/admin/v1/buckets/objbkt/object-tags",
		SetObjectTagsRequest{Key: "doc.txt", Tags: map[string]string{}})
	if rr.Code != http.StatusNoContent {
		t.Fatalf("clear status=%d", rr.Code)
	}
	tags, err = s.Meta.GetObjectTags(context.Background(), b.ID, "doc.txt", "")
	if err != nil {
		t.Fatalf("get tags after clear: %v", err)
	}
	if len(tags) != 0 {
		t.Errorf("tags after clear=%+v want empty", tags)
	}
}

func TestObjectTags_MissingKeyReturns400(t *testing.T) {
	s, _ := newObjectTestServer(t)
	rr := objectAdminRequest(t, s, http.MethodPut,
		"/admin/v1/buckets/objbkt/object-tags",
		map[string]any{"tags": map[string]string{"a": "b"}})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", rr.Code)
	}
}

func TestObjectTags_NoSuchKeyReturns404(t *testing.T) {
	s, _ := newObjectTestServer(t)
	rr := objectAdminRequest(t, s, http.MethodPut,
		"/admin/v1/buckets/objbkt/object-tags",
		SetObjectTagsRequest{Key: "missing.txt", Tags: map[string]string{}})
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d", rr.Code)
	}
}

func TestObjectRetention_RequiresObjectLockEnabled(t *testing.T) {
	s, b := newObjectTestServer(t)
	putObjectWithMeta(t, s, b, "doc.txt", nil)
	rr := objectAdminRequest(t, s, http.MethodPut,
		"/admin/v1/buckets/objbkt/object-retention",
		SetObjectRetentionRequest{
			Key:         "doc.txt",
			Mode:        meta.LockModeGovernance,
			RetainUntil: time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339),
		})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var er errorResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &er)
	if er.Code != "ObjectLockNotEnabled" {
		t.Errorf("code=%q want ObjectLockNotEnabled", er.Code)
	}
}

func TestObjectRetention_HappyAndRoundTrip(t *testing.T) {
	s, b := newObjectTestServer(t)
	if err := s.Meta.SetBucketObjectLockEnabled(context.Background(), b.Name, true); err != nil {
		t.Fatalf("enable lock: %v", err)
	}
	putObjectWithMeta(t, s, b, "doc.txt", nil)

	until := time.Now().Add(72 * time.Hour).UTC().Truncate(time.Second)
	rr := objectAdminRequest(t, s, http.MethodPut,
		"/admin/v1/buckets/objbkt/object-retention",
		SetObjectRetentionRequest{
			Key:         "doc.txt",
			Mode:        meta.LockModeCompliance,
			RetainUntil: until.Format(time.RFC3339),
		})
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}

	o, err := s.Meta.GetObject(context.Background(), b.ID, "doc.txt", "")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if o.RetainMode != meta.LockModeCompliance {
		t.Errorf("retain_mode=%q want COMPLIANCE", o.RetainMode)
	}
	if !o.RetainUntil.Equal(until) {
		t.Errorf("retain_until=%v want %v", o.RetainUntil, until)
	}

	// Mode "None" clears the retention.
	rr = objectAdminRequest(t, s, http.MethodPut,
		"/admin/v1/buckets/objbkt/object-retention",
		SetObjectRetentionRequest{Key: "doc.txt", Mode: "None"})
	if rr.Code != http.StatusNoContent {
		t.Fatalf("clear status=%d body=%s", rr.Code, rr.Body.String())
	}
	o, _ = s.Meta.GetObject(context.Background(), b.ID, "doc.txt", "")
	if o.RetainMode != "" || !o.RetainUntil.IsZero() {
		t.Errorf("retention not cleared: mode=%q until=%v", o.RetainMode, o.RetainUntil)
	}
}

func TestObjectRetention_BadModeRejected(t *testing.T) {
	s, b := newObjectTestServer(t)
	if err := s.Meta.SetBucketObjectLockEnabled(context.Background(), b.Name, true); err != nil {
		t.Fatalf("enable lock: %v", err)
	}
	putObjectWithMeta(t, s, b, "doc.txt", nil)
	rr := objectAdminRequest(t, s, http.MethodPut,
		"/admin/v1/buckets/objbkt/object-retention",
		SetObjectRetentionRequest{Key: "doc.txt", Mode: "FOREVER"})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", rr.Code)
	}
}

func TestObjectRetention_RetainUntilRequiredForMode(t *testing.T) {
	s, b := newObjectTestServer(t)
	if err := s.Meta.SetBucketObjectLockEnabled(context.Background(), b.Name, true); err != nil {
		t.Fatalf("enable lock: %v", err)
	}
	putObjectWithMeta(t, s, b, "doc.txt", nil)
	rr := objectAdminRequest(t, s, http.MethodPut,
		"/admin/v1/buckets/objbkt/object-retention",
		SetObjectRetentionRequest{Key: "doc.txt", Mode: meta.LockModeGovernance})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", rr.Code)
	}
}

func TestObjectLegalHold_HappyAndRoundTrip(t *testing.T) {
	s, b := newObjectTestServer(t)
	if err := s.Meta.SetBucketObjectLockEnabled(context.Background(), b.Name, true); err != nil {
		t.Fatalf("enable lock: %v", err)
	}
	putObjectWithMeta(t, s, b, "doc.txt", nil)

	rr := objectAdminRequest(t, s, http.MethodPut,
		"/admin/v1/buckets/objbkt/object-legal-hold",
		SetObjectLegalHoldRequest{Key: "doc.txt", Enabled: true})
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	o, _ := s.Meta.GetObject(context.Background(), b.ID, "doc.txt", "")
	if !o.LegalHold {
		t.Error("legal_hold not set")
	}

	// Clear: enabled=false works without ObjectLockEnabled requirement.
	rr = objectAdminRequest(t, s, http.MethodPut,
		"/admin/v1/buckets/objbkt/object-legal-hold",
		SetObjectLegalHoldRequest{Key: "doc.txt", Enabled: false})
	if rr.Code != http.StatusNoContent {
		t.Fatalf("clear status=%d", rr.Code)
	}
	o, _ = s.Meta.GetObject(context.Background(), b.ID, "doc.txt", "")
	if o.LegalHold {
		t.Error("legal_hold still set after clear")
	}
}

func TestObjectLegalHold_RequiresObjectLockEnabledForOn(t *testing.T) {
	s, b := newObjectTestServer(t)
	putObjectWithMeta(t, s, b, "doc.txt", nil)
	rr := objectAdminRequest(t, s, http.MethodPut,
		"/admin/v1/buckets/objbkt/object-legal-hold",
		SetObjectLegalHoldRequest{Key: "doc.txt", Enabled: true})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", rr.Code)
	}
	var er errorResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &er)
	if er.Code != "ObjectLockNotEnabled" {
		t.Errorf("code=%q", er.Code)
	}
}

func TestObjectVersions_ListsAllVersionsForKey(t *testing.T) {
	s, b := newObjectTestServer(t)
	if err := s.Meta.SetBucketVersioning(context.Background(), b.Name, meta.VersioningEnabled); err != nil {
		t.Fatalf("enable versioning: %v", err)
	}
	// Re-fetch bucket so the IsVersioningActive shortcut on PutObject takes
	// the versioned path.
	bv, err := s.Meta.GetBucket(context.Background(), b.Name)
	if err != nil {
		t.Fatalf("refetch: %v", err)
	}
	for i := range 3 {
		o := &meta.Object{
			BucketID:     bv.ID,
			Key:          "doc.txt",
			Size:         int64(10 + i),
			ETag:         "etag",
			StorageClass: "STANDARD",
			IsLatest:     true,
			Mtime:        time.Now().UTC().Add(time.Duration(i) * time.Second),
			Manifest:     &data.Manifest{},
		}
		if err := s.Meta.PutObject(context.Background(), o, true); err != nil {
			t.Fatalf("put v%d: %v", i, err)
		}
	}
	// Add an unrelated key — must NOT show up under doc.txt's versions.
	putObjectWithMeta(t, s, bv, "other.txt", nil)

	rr := objectAdminRequest(t, s, http.MethodGet,
		"/admin/v1/buckets/objbkt/object-versions?key=doc.txt", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got ObjectVersionsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Versions) != 3 {
		t.Fatalf("versions=%d want 3", len(got.Versions))
	}
	for _, v := range got.Versions {
		if v.VersionID == "" {
			t.Errorf("missing version_id: %+v", v)
		}
	}
	// Exactly one IsLatest=true.
	latest := 0
	for _, v := range got.Versions {
		if v.IsLatest {
			latest++
		}
	}
	if latest != 1 {
		t.Errorf("is_latest count=%d want 1", latest)
	}
}

func TestObjectVersions_BucketNotFoundReturns404(t *testing.T) {
	s, _ := newObjectTestServer(t)
	rr := objectAdminRequest(t, s, http.MethodGet,
		"/admin/v1/buckets/missing/object-versions?key=anything", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d", rr.Code)
	}
}

func TestObjectDelete_HappyProxiesS3DeleteObject(t *testing.T) {
	s, b := newObjectTestServer(t)
	putObjectWithMeta(t, s, b, "doc.txt", nil)

	rr := objectAdminRequest(t, s, http.MethodDelete,
		"/admin/v1/buckets/objbkt/objects/doc.txt", nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if _, err := s.Meta.GetObject(context.Background(), b.ID, "doc.txt", ""); err == nil {
		t.Error("object still present after delete")
	}
}

func TestObjectDelete_NestedKey(t *testing.T) {
	s, b := newObjectTestServer(t)
	putObjectWithMeta(t, s, b, "logs/2026/05.txt", nil)
	rr := objectAdminRequest(t, s, http.MethodDelete,
		"/admin/v1/buckets/objbkt/objects/logs/2026/05.txt", nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestObjectDelete_VersionedDelete(t *testing.T) {
	s, b := newObjectTestServer(t)
	if err := s.Meta.SetBucketVersioning(context.Background(), b.Name, meta.VersioningEnabled); err != nil {
		t.Fatalf("enable versioning: %v", err)
	}
	bv, _ := s.Meta.GetBucket(context.Background(), b.Name)
	o := &meta.Object{
		BucketID:     bv.ID,
		Key:          "doc.txt",
		Size:         11,
		ETag:         "etag",
		StorageClass: "STANDARD",
		IsLatest:     true,
		Mtime:        time.Now().UTC(),
		Manifest:     &data.Manifest{},
	}
	if err := s.Meta.PutObject(context.Background(), o, true); err != nil {
		t.Fatalf("put: %v", err)
	}
	versionID := o.VersionID
	if versionID == "" {
		t.Fatalf("expected version_id stamped on PutObject")
	}

	rr := objectAdminRequest(t, s, http.MethodDelete,
		"/admin/v1/buckets/objbkt/objects/doc.txt?versionId="+versionID, nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if _, err := s.Meta.GetObject(context.Background(), bv.ID, "doc.txt", versionID); err == nil {
		t.Error("version still present after delete")
	}
}
