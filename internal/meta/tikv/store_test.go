package tikv

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/gocql/gocql"

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/meta"
)

// newTestStore returns a Store backed by an in-process memBackend so the
// surface tests do not need a TiKV testcontainer (US-013 lands the
// contract suite that exercises the real txnkv path).
func newTestStore(t *testing.T) *Store {
	t.Helper()
	return openWithBackend(newMemBackend())
}

func TestProbeReady(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Probe(context.Background()); err != nil {
		t.Fatalf("Probe: %v", err)
	}
}

func TestCreateBucket(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	b, err := s.CreateBucket(ctx, "bkt", "alice", "STANDARD")
	if err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	if b.Name != "bkt" || b.Owner != "alice" || b.DefaultClass != "STANDARD" {
		t.Fatalf("bucket fields: %+v", b)
	}
	if b.Versioning != meta.VersioningDisabled {
		t.Fatalf("versioning default: %q", b.Versioning)
	}
	if b.ShardCount != defaultShardCount {
		t.Fatalf("shard count default: %d", b.ShardCount)
	}
	if b.ID.String() == "" {
		t.Fatalf("bucket id empty")
	}
}

func TestCreateBucketDuplicate(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	if _, err := s.CreateBucket(ctx, "bkt", "alice", "STANDARD"); err != nil {
		t.Fatalf("first create: %v", err)
	}
	_, err := s.CreateBucket(ctx, "bkt", "bob", "STANDARD")
	if !errors.Is(err, meta.ErrBucketAlreadyExists) {
		t.Fatalf("dup create: got %v, want ErrBucketAlreadyExists", err)
	}
}

func TestGetBucketRoundTrip(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	created, err := s.CreateBucket(ctx, "bkt", "alice", "STANDARD")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := s.GetBucket(ctx, "bkt")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != created.ID {
		t.Fatalf("id mismatch: got %v want %v", got.ID, created.ID)
	}
	if got.Owner != "alice" || got.DefaultClass != "STANDARD" {
		t.Fatalf("fields: %+v", got)
	}
}

func TestGetBucketMissing(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	if _, err := s.GetBucket(context.Background(), "ghost"); !errors.Is(err, meta.ErrBucketNotFound) {
		t.Fatalf("got %v, want ErrBucketNotFound", err)
	}
}

func TestSetBucketVersioning(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	if _, err := s.CreateBucket(ctx, "bkt", "alice", "STANDARD"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.SetBucketVersioning(ctx, "bkt", meta.VersioningEnabled); err != nil {
		t.Fatalf("set versioning: %v", err)
	}
	got, err := s.GetBucket(ctx, "bkt")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Versioning != meta.VersioningEnabled {
		t.Fatalf("versioning: got %q want Enabled", got.Versioning)
	}

	if err := s.SetBucketVersioning(ctx, "bkt", meta.VersioningSuspended); err != nil {
		t.Fatalf("set suspended: %v", err)
	}
	got, err = s.GetBucket(ctx, "bkt")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Versioning != meta.VersioningSuspended {
		t.Fatalf("versioning: got %q want Suspended", got.Versioning)
	}
}

func TestSetBucketVersioningMissing(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	err := s.SetBucketVersioning(context.Background(), "ghost", meta.VersioningEnabled)
	if !errors.Is(err, meta.ErrBucketNotFound) {
		t.Fatalf("got %v, want ErrBucketNotFound", err)
	}
}

func TestSetBucketAttrsIndependent(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	if _, err := s.CreateBucket(ctx, "bkt", "alice", "STANDARD"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.SetBucketACL(ctx, "bkt", "private"); err != nil {
		t.Fatalf("set acl: %v", err)
	}
	if err := s.SetBucketRegion(ctx, "bkt", "us-east-1"); err != nil {
		t.Fatalf("set region: %v", err)
	}
	if err := s.SetBucketObjectLockEnabled(ctx, "bkt", true); err != nil {
		t.Fatalf("set object lock: %v", err)
	}
	if err := s.SetBucketMfaDelete(ctx, "bkt", meta.MfaDeleteEnabled); err != nil {
		t.Fatalf("set mfa: %v", err)
	}
	got, err := s.GetBucket(ctx, "bkt")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ACL != "private" {
		t.Fatalf("acl: %q", got.ACL)
	}
	if got.Region != "us-east-1" {
		t.Fatalf("region: %q", got.Region)
	}
	if !got.ObjectLockEnabled {
		t.Fatalf("object lock not flipped")
	}
	if got.MfaDelete != meta.MfaDeleteEnabled {
		t.Fatalf("mfa delete: %q", got.MfaDelete)
	}
}

func TestListBuckets(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	for _, n := range []string{"alpha", "bravo", "charlie"} {
		owner := "alice"
		if n == "bravo" {
			owner = "bob"
		}
		if _, err := s.CreateBucket(ctx, n, owner, "STANDARD"); err != nil {
			t.Fatalf("create %q: %v", n, err)
		}
	}

	all, err := s.ListBuckets(ctx, "")
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	names := bucketNames(all)
	sort.Strings(names)
	if got, want := names, []string{"alpha", "bravo", "charlie"}; !equalStrings(got, want) {
		t.Fatalf("all names: %v want %v", got, want)
	}

	mine, err := s.ListBuckets(ctx, "alice")
	if err != nil {
		t.Fatalf("list mine: %v", err)
	}
	names = bucketNames(mine)
	sort.Strings(names)
	if got, want := names, []string{"alpha", "charlie"}; !equalStrings(got, want) {
		t.Fatalf("mine names: %v want %v", got, want)
	}
}

func TestDeleteBucketEmpty(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	if _, err := s.CreateBucket(ctx, "bkt", "alice", "STANDARD"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.DeleteBucket(ctx, "bkt"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.GetBucket(ctx, "bkt"); !errors.Is(err, meta.ErrBucketNotFound) {
		t.Fatalf("get after delete: %v want NotFound", err)
	}
}

func TestDeleteBucketMissing(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	if err := s.DeleteBucket(context.Background(), "ghost"); !errors.Is(err, meta.ErrBucketNotFound) {
		t.Fatalf("got %v, want ErrBucketNotFound", err)
	}
}

func TestDeleteBucketNotEmpty(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	b, err := s.CreateBucket(ctx, "bkt", "alice", "STANDARD")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Inject a bucket-scoped row directly via the kv backend so the
	// emptiness probe trips. Story US-004 lands real PutObject; this
	// test just needs *something* under PrefixForBucket.
	mb := s.kv.(*memBackend)
	mb.data[string(append(PrefixForBucket(b.ID), "o/test\x00\x00..."...))] = []byte("placeholder")

	err = s.DeleteBucket(ctx, "bkt")
	if !errors.Is(err, meta.ErrBucketNotEmpty) {
		t.Fatalf("got %v, want ErrBucketNotEmpty", err)
	}
}

func bucketNames(bs []*meta.Bucket) []string {
	out := make([]string, 0, len(bs))
	for _, b := range bs {
		out = append(out, b.Name)
	}
	return out
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

// ----------------------------------------------------------------------------
// Object CRUD (US-004).
// ----------------------------------------------------------------------------

func newVersionedBucket(t *testing.T, s *Store, name string) *meta.Bucket {
	t.Helper()
	ctx := context.Background()
	if _, err := s.CreateBucket(ctx, name, "alice", "STANDARD"); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	if err := s.SetBucketVersioning(ctx, name, meta.VersioningEnabled); err != nil {
		t.Fatalf("enable versioning: %v", err)
	}
	got, err := s.GetBucket(ctx, name)
	if err != nil {
		t.Fatalf("re-read bucket: %v", err)
	}
	return got
}

func putBody(t *testing.T, s *Store, b *meta.Bucket, key, body string, versioned bool) *meta.Object {
	t.Helper()
	o := &meta.Object{
		BucketID:     b.ID,
		Key:          key,
		StorageClass: "STANDARD",
		ETag:         body,
		Size:         int64(len(body)),
		Mtime:        time.Now().UTC(),
		Manifest:     &data.Manifest{Class: "STANDARD", Size: int64(len(body))},
	}
	if err := s.PutObject(context.Background(), o, versioned); err != nil {
		t.Fatalf("put %q: %v", body, err)
	}
	return o
}

func TestPutGetObjectNonVersioned(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	b, err := s.CreateBucket(ctx, "bkt", "alice", "STANDARD")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	o := putBody(t, s, b, "doc", "hello", false)
	if o.VersionID != meta.NullVersionID || !o.IsNull {
		t.Fatalf("non-versioned should map to null sentinel: %+v", o)
	}

	got, err := s.GetObject(ctx, b.ID, "doc", "")
	if err != nil {
		t.Fatalf("get latest: %v", err)
	}
	if got.ETag != "hello" || got.VersionID != meta.NullVersionID {
		t.Fatalf("got: %+v", got)
	}

	null, err := s.GetObject(ctx, b.ID, "doc", "null")
	if err != nil {
		t.Fatalf("get ?versionId=null: %v", err)
	}
	if null.ETag != "hello" {
		t.Fatalf("null literal lookup: %+v", null)
	}
}

func TestPutGetObjectVersionedOrdering(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	b := newVersionedBucket(t, s, "v")

	v1 := putBody(t, s, b, "doc", "one", true)
	v2 := putBody(t, s, b, "doc", "two", true)
	v3 := putBody(t, s, b, "doc", "three", true)
	if v1.VersionID == v2.VersionID || v2.VersionID == v3.VersionID {
		t.Fatalf("version ids not distinct")
	}

	latest, err := s.GetObject(ctx, b.ID, "doc", "")
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if latest.VersionID != v3.VersionID {
		t.Fatalf("latest should be v3 (%s), got %s", v3.VersionID, latest.VersionID)
	}

	old, err := s.GetObject(ctx, b.ID, "doc", v1.VersionID)
	if err != nil {
		t.Fatalf("v1 lookup: %v", err)
	}
	if old.ETag != "one" {
		t.Fatalf("v1 etag: %+v", old)
	}
}

func TestNonVersionedPutOverwritesAllPriorVersions(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	b := newVersionedBucket(t, s, "mix")
	v1 := putBody(t, s, b, "doc", "one", true)
	v2 := putBody(t, s, b, "doc", "two", true)
	// Switch to non-versioned write — every prior version row must be wiped.
	if err := s.SetBucketVersioning(ctx, "mix", meta.VersioningDisabled); err != nil {
		t.Fatalf("disable versioning: %v", err)
	}
	cur := putBody(t, s, b, "doc", "fresh", false)

	if got, err := s.GetObject(ctx, b.ID, "doc", v1.VersionID); !errors.Is(err, meta.ErrObjectNotFound) {
		t.Fatalf("v1 should be wiped: %v %+v", err, got)
	}
	if got, err := s.GetObject(ctx, b.ID, "doc", v2.VersionID); !errors.Is(err, meta.ErrObjectNotFound) {
		t.Fatalf("v2 should be wiped: %v %+v", err, got)
	}
	if got, err := s.GetObject(ctx, b.ID, "doc", ""); err != nil || got.ETag != "fresh" {
		t.Fatalf("latest after non-versioned put: %v %+v", err, got)
	}
	if got, err := s.GetObject(ctx, b.ID, "doc", cur.VersionID); err != nil || got.ETag != "fresh" {
		t.Fatalf("by-version after non-versioned put: %v %+v", err, got)
	}
}

func TestDeleteObjectVersioned(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	b := newVersionedBucket(t, s, "vd")
	v1 := putBody(t, s, b, "k", "x", true)

	dm, err := s.DeleteObject(ctx, b.ID, "k", "", true)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if dm == nil || !dm.IsDeleteMarker {
		t.Fatalf("expected delete marker, got %+v", dm)
	}
	if _, err := s.GetObject(ctx, b.ID, "k", ""); !errors.Is(err, meta.ErrObjectNotFound) {
		t.Fatalf("after delete marker, latest must hide: %v", err)
	}
	got, err := s.GetObject(ctx, b.ID, "k", v1.VersionID)
	if err != nil || got.ETag != "x" {
		t.Fatalf("prior version still readable: %v %+v", err, got)
	}
}

func TestDeleteObjectByVersion(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	b := newVersionedBucket(t, s, "vp")
	v1 := putBody(t, s, b, "k", "one", true)
	v2 := putBody(t, s, b, "k", "two", true)

	prior, err := s.DeleteObject(ctx, b.ID, "k", v1.VersionID, true)
	if err != nil || prior.ETag != "one" {
		t.Fatalf("delete v1: %v %+v", err, prior)
	}
	if _, err := s.GetObject(ctx, b.ID, "k", v1.VersionID); !errors.Is(err, meta.ErrObjectNotFound) {
		t.Fatalf("v1 should be gone: %v", err)
	}
	got, err := s.GetObject(ctx, b.ID, "k", "")
	if err != nil || got.VersionID != v2.VersionID {
		t.Fatalf("latest after deleting v1 should still be v2: %v %+v", err, got)
	}
}

func TestDeleteObjectNonVersioned(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	b, err := s.CreateBucket(ctx, "nv", "alice", "STANDARD")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	putBody(t, s, b, "k", "x", false)

	prior, err := s.DeleteObject(ctx, b.ID, "k", "", false)
	if err != nil || prior == nil || prior.ETag != "x" {
		t.Fatalf("delete: %v %+v", err, prior)
	}
	if _, err := s.GetObject(ctx, b.ID, "k", ""); !errors.Is(err, meta.ErrObjectNotFound) {
		t.Fatalf("after non-versioned delete, latest gone: %v", err)
	}
	if _, err := s.DeleteObject(ctx, b.ID, "k", "", false); !errors.Is(err, meta.ErrObjectNotFound) {
		t.Fatalf("second delete should be NotFound: %v", err)
	}
}

func TestDeleteObjectNullReplacement(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	b := newVersionedBucket(t, s, "sus")

	v1 := putBody(t, s, b, "k", "old", true) // TimeUUID-versioned
	// Suspend → write a null-versioned PUT, then a null-replacement DELETE.
	if err := s.SetBucketVersioning(ctx, "sus", meta.VersioningSuspended); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	nullPut := &meta.Object{
		BucketID:     b.ID,
		Key:          "k",
		IsNull:       true,
		StorageClass: "STANDARD",
		ETag:         "null-body",
		Size:         9,
		Mtime:        time.Now().UTC(),
		Manifest:     &data.Manifest{Class: "STANDARD"},
	}
	if err := s.PutObject(ctx, nullPut, true); err != nil {
		t.Fatalf("null put: %v", err)
	}

	marker, err := s.DeleteObjectNullReplacement(ctx, b.ID, "k")
	if err != nil {
		t.Fatalf("null replacement: %v", err)
	}
	if !marker.IsDeleteMarker || !marker.IsNull || marker.VersionID != meta.NullVersionID {
		t.Fatalf("marker shape: %+v", marker)
	}

	// Explicit-version lookups: marker is addressable as ?versionId=null,
	// the TimeUUID-versioned sibling is preserved.
	gotMarker, err := s.GetObject(ctx, b.ID, "k", "null")
	if err != nil || !gotMarker.IsDeleteMarker || !gotMarker.IsNull {
		t.Fatalf("explicit null lookup should return marker: %v %+v", err, gotMarker)
	}
	old, err := s.GetObject(ctx, b.ID, "k", v1.VersionID)
	if err != nil || old.ETag != "old" {
		t.Fatalf("TimeUUID version preserved: %v %+v", err, old)
	}
}

func TestSetObjectStorageCAS(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	b, _ := s.CreateBucket(ctx, "cas", "alice", "STANDARD")
	o := putBody(t, s, b, "k", "x", false)

	newMan := &data.Manifest{Class: "STANDARD_IA"}
	applied, err := s.SetObjectStorage(ctx, b.ID, "k", o.VersionID, "STANDARD", "STANDARD_IA", newMan)
	if err != nil || !applied {
		t.Fatalf("CAS happy: applied=%v err=%v", applied, err)
	}

	// CAS must reject when the prior class no longer matches.
	applied, err = s.SetObjectStorage(ctx, b.ID, "k", o.VersionID, "STANDARD", "GLACIER_IR", newMan)
	if err != nil {
		t.Fatalf("CAS reject err: %v", err)
	}
	if applied {
		t.Fatalf("CAS should reject after class flipped to STANDARD_IA")
	}

	// Unconditional update (empty expected) wins.
	applied, err = s.SetObjectStorage(ctx, b.ID, "k", o.VersionID, "", "GLACIER_IR", newMan)
	if err != nil || !applied {
		t.Fatalf("unconditional update: applied=%v err=%v", applied, err)
	}
	got, _ := s.GetObject(ctx, b.ID, "k", o.VersionID)
	if got.StorageClass != "GLACIER_IR" {
		t.Fatalf("storage class after unconditional flip: %q", got.StorageClass)
	}
}

func TestSetObjectStorageMissing(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	b, _ := s.CreateBucket(ctx, "miss", "alice", "STANDARD")
	if _, err := s.SetObjectStorage(ctx, b.ID, "ghost", gocql.TimeUUID().String(), "", "STANDARD_IA", nil); !errors.Is(err, meta.ErrObjectNotFound) {
		t.Fatalf("missing object: %v want NotFound", err)
	}
}

func TestObjectKeyVersionsCoexist(t *testing.T) {
	// Two keys whose escaped forms have a 0x00 byte must not collide on the
	// version-DESC suffix lookup. Mirrors the property tested in keys_test.go
	// at the keyspace level — this exercises it through the storage layer.
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	b := newVersionedBucket(t, s, "co")

	a := putBody(t, s, b, "foo", "a-body", true)
	bObj := putBody(t, s, b, "foo\x00bar", "b-body", true)

	gotA, err := s.GetObject(context.Background(), b.ID, "foo", "")
	if err != nil || gotA.VersionID != a.VersionID || gotA.ETag != "a-body" {
		t.Fatalf("foo: %v %+v", err, gotA)
	}
	gotB, err := s.GetObject(context.Background(), b.ID, "foo\x00bar", "")
	if err != nil || gotB.VersionID != bObj.VersionID || gotB.ETag != "b-body" {
		t.Fatalf("foo\\x00bar: %v %+v", err, gotB)
	}
}

// ----------------------------------------------------------------------------
// ListObjects + ListObjectVersions (US-005).
// ----------------------------------------------------------------------------

func TestListObjectsBasic(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	b, _ := s.CreateBucket(ctx, "bkt", "alice", "STANDARD")

	for _, k := range []string{"a", "b", "c"} {
		putBody(t, s, b, k, k, false)
	}

	res, err := s.ListObjects(ctx, b.ID, meta.ListOptions{Limit: 1000})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if got := keysOf(res.Objects); !equalStrings(got, []string{"a", "b", "c"}) {
		t.Fatalf("keys: %v", got)
	}
	if res.Truncated {
		t.Fatalf("should not be truncated")
	}
}

func TestListObjectsHidesDeleteMarkers(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	b := newVersionedBucket(t, s, "lst")

	putBody(t, s, b, "a", "1", true)
	putBody(t, s, b, "b", "1", true)
	putBody(t, s, b, "c", "1", true)
	if _, err := s.DeleteObject(ctx, b.ID, "b", "", true); err != nil {
		t.Fatalf("delete: %v", err)
	}

	res, err := s.ListObjects(ctx, b.ID, meta.ListOptions{Limit: 1000})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if got := keysOf(res.Objects); !equalStrings(got, []string{"a", "c"}) {
		t.Fatalf("keys: %v want [a c]", got)
	}

	versions, err := s.ListObjectVersions(ctx, b.ID, meta.ListOptions{Limit: 1000})
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	dm := false
	for _, v := range versions.Versions {
		if v.IsDeleteMarker {
			dm = true
		}
	}
	if !dm {
		t.Fatalf("ListObjectVersions should include delete markers")
	}
}

func TestListObjectsLatestPerKey(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	b := newVersionedBucket(t, s, "lat")

	putBody(t, s, b, "doc", "v1", true)
	putBody(t, s, b, "doc", "v2", true)
	v3 := putBody(t, s, b, "doc", "v3", true)

	res, err := s.ListObjects(ctx, b.ID, meta.ListOptions{Limit: 1000})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(res.Objects) != 1 {
		t.Fatalf("expected 1 object, got %d", len(res.Objects))
	}
	if res.Objects[0].VersionID != v3.VersionID || res.Objects[0].ETag != "v3" {
		t.Fatalf("latest row: %+v want v3", res.Objects[0])
	}
	if !res.Objects[0].IsLatest {
		t.Fatalf("IsLatest must be true")
	}
}

func TestListObjectsPrefix(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	b, _ := s.CreateBucket(ctx, "p", "alice", "STANDARD")

	for _, k := range []string{"foo/a", "foo/b", "bar/a", "baz"} {
		putBody(t, s, b, k, k, false)
	}

	res, err := s.ListObjects(ctx, b.ID, meta.ListOptions{Prefix: "foo/", Limit: 1000})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if got := keysOf(res.Objects); !equalStrings(got, []string{"foo/a", "foo/b"}) {
		t.Fatalf("prefix: %v want [foo/a foo/b]", got)
	}
}

func TestListObjectsDelimiter(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	b, _ := s.CreateBucket(ctx, "d", "alice", "STANDARD")

	for _, k := range []string{"a/x", "a/y", "b/z", "c"} {
		putBody(t, s, b, k, k, false)
	}

	res, err := s.ListObjects(ctx, b.ID, meta.ListOptions{Delimiter: "/", Limit: 1000})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if got := keysOf(res.Objects); !equalStrings(got, []string{"c"}) {
		t.Fatalf("objects: %v want [c]", got)
	}
	prefixes := append([]string(nil), res.CommonPrefixes...)
	sort.Strings(prefixes)
	if !equalStrings(prefixes, []string{"a/", "b/"}) {
		t.Fatalf("prefixes: %v want [a/ b/]", prefixes)
	}
}

func TestListObjectsMarkerExclusive(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	b, _ := s.CreateBucket(ctx, "m", "alice", "STANDARD")

	for _, k := range []string{"a", "b", "c", "d"} {
		putBody(t, s, b, k, k, false)
	}

	res, err := s.ListObjects(ctx, b.ID, meta.ListOptions{Marker: "b", Limit: 1000})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if got := keysOf(res.Objects); !equalStrings(got, []string{"c", "d"}) {
		t.Fatalf("after-marker: %v want [c d]", got)
	}
}

func TestListObjectsTruncation(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	b, _ := s.CreateBucket(ctx, "pg", "alice", "STANDARD")

	for _, k := range []string{"a", "b", "c", "d", "e"} {
		putBody(t, s, b, k, k, false)
	}

	res, err := s.ListObjects(ctx, b.ID, meta.ListOptions{Limit: 2})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !res.Truncated {
		t.Fatalf("should be truncated")
	}
	if got := keysOf(res.Objects); !equalStrings(got, []string{"a", "b"}) {
		t.Fatalf("page1 keys: %v", got)
	}
	// NextMarker is the next-not-emitted key — matches the
	// memory/Cassandra shape for the gateway's pagination handler.
	if res.NextMarker != "c" {
		t.Fatalf("next marker: %q want c", res.NextMarker)
	}
}

func TestListObjectVersionsAllRows(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	b := newVersionedBucket(t, s, "lv")

	v1 := putBody(t, s, b, "doc", "v1", true)
	v2 := putBody(t, s, b, "doc", "v2", true)
	putBody(t, s, b, "other", "x", true)

	res, err := s.ListObjectVersions(ctx, b.ID, meta.ListOptions{Limit: 1000})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(res.Versions) != 3 {
		t.Fatalf("versions: %d want 3", len(res.Versions))
	}

	// Versions ordered (key ASC, version DESC). doc/v2 first with IsLatest,
	// doc/v1 second without, then other.
	if res.Versions[0].Key != "doc" || res.Versions[0].VersionID != v2.VersionID || !res.Versions[0].IsLatest {
		t.Fatalf("v0: %+v", res.Versions[0])
	}
	if res.Versions[1].Key != "doc" || res.Versions[1].VersionID != v1.VersionID || res.Versions[1].IsLatest {
		t.Fatalf("v1: %+v", res.Versions[1])
	}
	if res.Versions[2].Key != "other" || !res.Versions[2].IsLatest {
		t.Fatalf("v2: %+v", res.Versions[2])
	}
}

func TestListObjectVersionsPagination(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	b := newVersionedBucket(t, s, "lvp")

	putBody(t, s, b, "a", "1", true)
	putBody(t, s, b, "a", "2", true)
	putBody(t, s, b, "b", "1", true)

	res, err := s.ListObjectVersions(ctx, b.ID, meta.ListOptions{Limit: 2})
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if !res.Truncated || len(res.Versions) != 2 {
		t.Fatalf("page1: %+v", res)
	}
	if res.NextKeyMarker == "" || res.NextVersionID == "" {
		t.Fatalf("page1 next: %q %q", res.NextKeyMarker, res.NextVersionID)
	}
}

func keysOf(objs []*meta.Object) []string {
	out := make([]string, 0, len(objs))
	for _, o := range objs {
		out = append(out, o.Key)
	}
	return out
}

// ----------------------------------------------------------------------------
// TestPrefixEnd guards the helper used by every range scan.
func TestPrefixEnd(t *testing.T) {
	cases := []struct {
		in, want []byte
	}{
		{[]byte("a"), []byte("b")},
		{[]byte("ab"), []byte("ac")},
		{[]byte{0x00}, []byte{0x01}},
		{[]byte{0xFF}, nil},
		{[]byte{0x01, 0xFF}, []byte{0x02}},
	}
	for _, c := range cases {
		got := prefixEnd(c.in)
		if string(got) != string(c.want) {
			t.Fatalf("prefixEnd(%v)=%v want %v", c.in, got, c.want)
		}
	}
}
