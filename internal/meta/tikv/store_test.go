package tikv

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/gocql/gocql"
	"github.com/google/uuid"

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
// Multipart upload lifecycle (US-006).
// ----------------------------------------------------------------------------

func newMultipart(t *testing.T, s *Store, b *meta.Bucket, key string) *meta.MultipartUpload {
	t.Helper()
	mu := &meta.MultipartUpload{
		BucketID:     b.ID,
		UploadID:     gocql.TimeUUID().String(),
		Key:          key,
		StorageClass: "STANDARD",
		ContentType:  "application/octet-stream",
		InitiatedAt:  time.Now().UTC(),
	}
	if err := s.CreateMultipartUpload(context.Background(), mu); err != nil {
		t.Fatalf("create multipart: %v", err)
	}
	return mu
}

func savePart(t *testing.T, s *Store, b *meta.Bucket, uploadID string, num int, etag string, size int64) {
	t.Helper()
	part := &meta.MultipartPart{
		PartNumber: num,
		ETag:       etag,
		Size:       size,
		Manifest: &data.Manifest{
			Class:  "STANDARD",
			Size:   size,
			Chunks: []data.ChunkRef{{Cluster: "c", Pool: "p", OID: etag, Size: size}},
		},
	}
	if err := s.SavePart(context.Background(), b.ID, uploadID, part); err != nil {
		t.Fatalf("save part %d: %v", num, err)
	}
}

func TestMultipartCreateAndGet(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	b, _ := s.CreateBucket(ctx, "mp", "alice", "STANDARD")
	mu := newMultipart(t, s, b, "obj")

	got, err := s.GetMultipartUpload(ctx, b.ID, mu.UploadID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Key != "obj" || got.Status != multipartStatusUploading {
		t.Fatalf("got: %+v", got)
	}
}

func TestMultipartGetMissing(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	b, _ := s.CreateBucket(ctx, "mp", "alice", "STANDARD")
	if _, err := s.GetMultipartUpload(ctx, b.ID, gocql.TimeUUID().String()); !errors.Is(err, meta.ErrMultipartNotFound) {
		t.Fatalf("got %v want ErrMultipartNotFound", err)
	}
}

func TestMultipartListByPrefix(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	b, _ := s.CreateBucket(ctx, "mp", "alice", "STANDARD")
	newMultipart(t, s, b, "foo/a")
	newMultipart(t, s, b, "foo/b")
	newMultipart(t, s, b, "bar")

	all, err := s.ListMultipartUploads(ctx, b.ID, "", 1000)
	if err != nil || len(all) != 3 {
		t.Fatalf("all: %v len=%d", err, len(all))
	}
	pref, err := s.ListMultipartUploads(ctx, b.ID, "foo/", 1000)
	if err != nil || len(pref) != 2 {
		t.Fatalf("prefix: %v len=%d", err, len(pref))
	}
}

func TestMultipartSavePartListParts(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	b, _ := s.CreateBucket(ctx, "mp", "alice", "STANDARD")
	mu := newMultipart(t, s, b, "obj")

	savePart(t, s, b, mu.UploadID, 2, "etag2", 200)
	savePart(t, s, b, mu.UploadID, 1, "etag1", 100)
	savePart(t, s, b, mu.UploadID, 3, "etag3", 300)

	parts, err := s.ListParts(ctx, b.ID, mu.UploadID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(parts) != 3 {
		t.Fatalf("len: %d want 3", len(parts))
	}
	for i, want := range []int{1, 2, 3} {
		if parts[i].PartNumber != want {
			t.Fatalf("part %d: got %d want %d (4-byte BE encoding should sort ascending)", i, parts[i].PartNumber, want)
		}
	}
}

func TestMultipartSavePartMissingUpload(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	b, _ := s.CreateBucket(ctx, "mp", "alice", "STANDARD")
	part := &meta.MultipartPart{PartNumber: 1, ETag: "x", Size: 1}
	if err := s.SavePart(ctx, b.ID, gocql.TimeUUID().String(), part); !errors.Is(err, meta.ErrMultipartNotFound) {
		t.Fatalf("got %v want ErrMultipartNotFound", err)
	}
}

func TestMultipartCompleteHappy(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	b, _ := s.CreateBucket(ctx, "mp", "alice", "STANDARD")
	mu := newMultipart(t, s, b, "obj")

	savePart(t, s, b, mu.UploadID, 1, "e1", 100)
	savePart(t, s, b, mu.UploadID, 2, "e2", 200)

	obj := &meta.Object{
		BucketID:     b.ID,
		Key:          mu.Key,
		StorageClass: "STANDARD",
		ETag:         "final-etag",
	}
	orphans, err := s.CompleteMultipartUpload(ctx, obj, mu.UploadID, []meta.CompletePart{
		{PartNumber: 1, ETag: "e1"},
		{PartNumber: 2, ETag: "e2"},
	}, false)
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if len(orphans) != 0 {
		t.Fatalf("orphans: %d want 0", len(orphans))
	}

	// Multipart upload + part rows must be gone after Complete.
	if _, err := s.GetMultipartUpload(ctx, b.ID, mu.UploadID); !errors.Is(err, meta.ErrMultipartNotFound) {
		t.Fatalf("upload row still present: %v", err)
	}

	// Final object row materialised under the latest version.
	got, err := s.GetObject(ctx, b.ID, "obj", "")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Size != 300 || got.ETag != "final-etag" {
		t.Fatalf("got: %+v", got)
	}
	if got.PartsCount != 0 || len(got.PartSizes) != 2 || got.PartSizes[0] != 100 || got.PartSizes[1] != 200 {
		t.Fatalf("part sizes: %+v", got.PartSizes)
	}
}

func TestMultipartCompleteOrphans(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	b, _ := s.CreateBucket(ctx, "mp", "alice", "STANDARD")
	mu := newMultipart(t, s, b, "obj")

	savePart(t, s, b, mu.UploadID, 1, "e1", 100)
	savePart(t, s, b, mu.UploadID, 2, "e2", 200)
	savePart(t, s, b, mu.UploadID, 3, "stale", 50)

	obj := &meta.Object{BucketID: b.ID, Key: mu.Key, StorageClass: "STANDARD", ETag: "x"}
	orphans, err := s.CompleteMultipartUpload(ctx, obj, mu.UploadID, []meta.CompletePart{
		{PartNumber: 1, ETag: "e1"},
		{PartNumber: 2, ETag: "e2"},
	}, false)
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if len(orphans) != 1 {
		t.Fatalf("orphans: %d want 1 (part 3 unused)", len(orphans))
	}
}

func TestMultipartCompleteEtagMismatch(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	b, _ := s.CreateBucket(ctx, "mp", "alice", "STANDARD")
	mu := newMultipart(t, s, b, "obj")
	savePart(t, s, b, mu.UploadID, 1, "real", 100)

	obj := &meta.Object{BucketID: b.ID, Key: mu.Key, StorageClass: "STANDARD"}
	_, err := s.CompleteMultipartUpload(ctx, obj, mu.UploadID, []meta.CompletePart{
		{PartNumber: 1, ETag: "wrong"},
	}, false)
	if !errors.Is(err, meta.ErrMultipartETagMismatch) {
		t.Fatalf("got %v want ErrMultipartETagMismatch", err)
	}
}

func TestMultipartCompleteMissingPart(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	b, _ := s.CreateBucket(ctx, "mp", "alice", "STANDARD")
	mu := newMultipart(t, s, b, "obj")
	savePart(t, s, b, mu.UploadID, 1, "e1", 100)

	obj := &meta.Object{BucketID: b.ID, Key: mu.Key, StorageClass: "STANDARD"}
	_, err := s.CompleteMultipartUpload(ctx, obj, mu.UploadID, []meta.CompletePart{
		{PartNumber: 1, ETag: "e1"},
		{PartNumber: 2, ETag: "e2"},
	}, false)
	if !errors.Is(err, meta.ErrMultipartPartMissing) {
		t.Fatalf("got %v want ErrMultipartPartMissing", err)
	}
}

func TestMultipartCompleteSecondCallInProgress(t *testing.T) {
	// Mirrors Cassandra LWT — once the first Complete flips status to
	// 'completing' the second call observes ErrMultipartInProgress instead
	// of racing to write the object row twice.
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	b, _ := s.CreateBucket(ctx, "mp", "alice", "STANDARD")
	mu := newMultipart(t, s, b, "obj")
	savePart(t, s, b, mu.UploadID, 1, "e1", 10)

	// Manually flip status to 'completing' to simulate "first Complete is
	// mid-flight". Inject directly via the kv backend.
	mb := s.kv.(*memBackend)
	muRow := *mu
	muRow.Status = multipartStatusCompleting
	payload, err := encodeMultipart(&muRow)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	mb.data[string(MultipartKey(b.ID, mu.UploadID))] = payload

	obj := &meta.Object{BucketID: b.ID, Key: mu.Key, StorageClass: "STANDARD"}
	_, err = s.CompleteMultipartUpload(ctx, obj, mu.UploadID, []meta.CompletePart{
		{PartNumber: 1, ETag: "e1"},
	}, false)
	if !errors.Is(err, meta.ErrMultipartInProgress) {
		t.Fatalf("got %v want ErrMultipartInProgress", err)
	}
}

func TestMultipartCompleteRaceOneWinner(t *testing.T) {
	// Two concurrent CompleteMultipartUpload calls against the same uploadID:
	// the pessimistic txn LockKeys serialises them, the loser observes
	// status='completing' (from the winner's flip) and returns
	// ErrMultipartInProgress. Mirror of the Cassandra LWT race coverage
	// already in the contract suite.
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	b, _ := s.CreateBucket(ctx, "mp", "alice", "STANDARD")
	mu := newMultipart(t, s, b, "obj")
	savePart(t, s, b, mu.UploadID, 1, "e1", 10)

	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			obj := &meta.Object{BucketID: b.ID, Key: mu.Key, StorageClass: "STANDARD"}
			_, err := s.CompleteMultipartUpload(ctx, obj, mu.UploadID, []meta.CompletePart{
				{PartNumber: 1, ETag: "e1"},
			}, false)
			results <- err
		}()
	}
	var winners, losers int
	for i := 0; i < 2; i++ {
		err := <-results
		switch {
		case err == nil:
			winners++
		case errors.Is(err, meta.ErrMultipartInProgress), errors.Is(err, meta.ErrMultipartNotFound):
			// NotFound is also acceptable: by the time the loser starts, the
			// winner committed and removed the upload row.
			losers++
		default:
			t.Fatalf("unexpected err: %v", err)
		}
	}
	if winners != 1 || losers != 1 {
		t.Fatalf("expected exactly one winner, got winners=%d losers=%d", winners, losers)
	}
}

func TestMultipartCompleteVersioned(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	b := newVersionedBucket(t, s, "mpv")
	mu := newMultipart(t, s, b, "obj")
	savePart(t, s, b, mu.UploadID, 1, "e1", 10)

	obj := &meta.Object{BucketID: b.ID, Key: mu.Key, StorageClass: "STANDARD"}
	if _, err := s.CompleteMultipartUpload(ctx, obj, mu.UploadID, []meta.CompletePart{
		{PartNumber: 1, ETag: "e1"},
	}, true); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if obj.VersionID == "" || obj.VersionID == meta.NullVersionID {
		t.Fatalf("versioned bucket should mint a TimeUUID: %q", obj.VersionID)
	}
}

func TestMultipartAbort(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	b, _ := s.CreateBucket(ctx, "mp", "alice", "STANDARD")
	mu := newMultipart(t, s, b, "obj")
	savePart(t, s, b, mu.UploadID, 1, "e1", 100)
	savePart(t, s, b, mu.UploadID, 2, "e2", 200)

	manifests, err := s.AbortMultipartUpload(ctx, b.ID, mu.UploadID)
	if err != nil {
		t.Fatalf("abort: %v", err)
	}
	if len(manifests) != 2 {
		t.Fatalf("manifests: %d want 2", len(manifests))
	}

	if _, err := s.GetMultipartUpload(ctx, b.ID, mu.UploadID); !errors.Is(err, meta.ErrMultipartNotFound) {
		t.Fatalf("upload row still present: %v", err)
	}
	// Second abort is NotFound (idempotent).
	if _, err := s.AbortMultipartUpload(ctx, b.ID, mu.UploadID); !errors.Is(err, meta.ErrMultipartNotFound) {
		t.Fatalf("second abort: %v want ErrMultipartNotFound", err)
	}
}

func TestMultipartCompletionRecord(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	b, _ := s.CreateBucket(ctx, "mc", "alice", "STANDARD")
	uploadID := gocql.TimeUUID().String()
	rec := &meta.MultipartCompletion{
		BucketID:    b.ID,
		UploadID:    uploadID,
		Key:         "k",
		ETag:        "etag",
		VersionID:   "v",
		Body:        []byte("body"),
		Headers:     map[string]string{"ETag": `"etag"`},
		CompletedAt: time.Now().UTC(),
	}
	if err := s.RecordMultipartCompletion(ctx, rec, time.Hour); err != nil {
		t.Fatalf("record: %v", err)
	}
	got, err := s.GetMultipartCompletion(ctx, b.ID, uploadID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ETag != "etag" || string(got.Body) != "body" || got.Headers["ETag"] != `"etag"` {
		t.Fatalf("got: %+v", got)
	}
	if _, err := s.GetMultipartCompletion(ctx, b.ID, gocql.TimeUUID().String()); !errors.Is(err, meta.ErrMultipartCompletionNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func TestMultipartCompletionExpired(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	b, _ := s.CreateBucket(ctx, "mc", "alice", "STANDARD")
	uploadID := gocql.TimeUUID().String()
	rec := &meta.MultipartCompletion{
		BucketID: b.ID,
		UploadID: uploadID,
		Key:      "k",
	}
	// Record with negative TTL → ExpiresAt is in the past, GET must lazy-expire.
	if err := s.RecordMultipartCompletion(ctx, rec, -time.Hour); err != nil {
		t.Fatalf("record: %v", err)
	}
	if _, err := s.GetMultipartCompletion(ctx, b.ID, uploadID); !errors.Is(err, meta.ErrMultipartCompletionNotFound) {
		t.Fatalf("expired: %v want ErrMultipartCompletionNotFound", err)
	}
}

func TestMultipartUpdateSSEWrap(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	b, _ := s.CreateBucket(ctx, "mp", "alice", "STANDARD")
	mu := newMultipart(t, s, b, "obj")

	if err := s.UpdateMultipartUploadSSEWrap(ctx, b.ID, mu.UploadID, []byte("wrapped"), "kid"); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, err := s.GetMultipartUpload(ctx, b.ID, mu.UploadID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(got.SSEKey) != "wrapped" || got.SSEKeyID != "kid" {
		t.Fatalf("got: %+v", got)
	}

	// Missing upload → NotFound.
	if err := s.UpdateMultipartUploadSSEWrap(ctx, b.ID, gocql.TimeUUID().String(), []byte("x"), "k"); !errors.Is(err, meta.ErrMultipartNotFound) {
		t.Fatalf("missing: %v want NotFound", err)
	}
}

// ----------------------------------------------------------------------------
// US-007 — bucket-level config blob round trips.
//
// Each per-config wrapper (lifecycle, CORS, policy, ...) routes through the
// shared setBucketBlob/getBucketBlob/deleteBucketBlob trio. Per-blob tests
// would mostly duplicate, so the table-test below covers every kind in one
// pass, asserting:
//
//   - GetBucketX on a missing row returns the right ErrNoSuchX sentinel.
//   - Set then Get round-trips the bytes verbatim.
//   - Delete then Get returns the missing-sentinel again (idempotent re-delete
//     remains nil).
//   - Per-bucket isolation: the same kind under a different bucketID returns
//     missing.
type blobOps struct {
	name    string
	missing error
	set     func(s *Store, ctx context.Context, id uuid.UUID, blob []byte) error
	get     func(s *Store, ctx context.Context, id uuid.UUID) ([]byte, error)
	del     func(s *Store, ctx context.Context, id uuid.UUID) error
}

func bucketBlobOps() []blobOps {
	return []blobOps{
		{"lifecycle", meta.ErrNoSuchLifecycle, (*Store).SetBucketLifecycle, (*Store).GetBucketLifecycle, (*Store).DeleteBucketLifecycle},
		{"cors", meta.ErrNoSuchCORS, (*Store).SetBucketCORS, (*Store).GetBucketCORS, (*Store).DeleteBucketCORS},
		{"policy", meta.ErrNoSuchBucketPolicy, (*Store).SetBucketPolicy, (*Store).GetBucketPolicy, (*Store).DeleteBucketPolicy},
		{"public-access-block", meta.ErrNoSuchPublicAccessBlock, (*Store).SetBucketPublicAccessBlock, (*Store).GetBucketPublicAccessBlock, (*Store).DeleteBucketPublicAccessBlock},
		{"ownership-controls", meta.ErrNoSuchOwnershipControls, (*Store).SetBucketOwnershipControls, (*Store).GetBucketOwnershipControls, (*Store).DeleteBucketOwnershipControls},
		{"encryption", meta.ErrNoSuchEncryption, (*Store).SetBucketEncryption, (*Store).GetBucketEncryption, (*Store).DeleteBucketEncryption},
		{"object-lock", meta.ErrNoSuchObjectLockConfig, (*Store).SetBucketObjectLockConfig, (*Store).GetBucketObjectLockConfig, (*Store).DeleteBucketObjectLockConfig},
		{"notification", meta.ErrNoSuchNotification, (*Store).SetBucketNotificationConfig, (*Store).GetBucketNotificationConfig, (*Store).DeleteBucketNotificationConfig},
		{"website", meta.ErrNoSuchWebsite, (*Store).SetBucketWebsite, (*Store).GetBucketWebsite, (*Store).DeleteBucketWebsite},
		{"replication", meta.ErrNoSuchReplication, (*Store).SetBucketReplication, (*Store).GetBucketReplication, (*Store).DeleteBucketReplication},
		{"logging", meta.ErrNoSuchLogging, (*Store).SetBucketLogging, (*Store).GetBucketLogging, (*Store).DeleteBucketLogging},
		{"tagging", meta.ErrNoSuchTagSet, (*Store).SetBucketTagging, (*Store).GetBucketTagging, (*Store).DeleteBucketTagging},
	}
}

func TestBucketBlobRoundTrip(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	b1, err := s.CreateBucket(ctx, "bkt1", "alice", "STANDARD")
	if err != nil {
		t.Fatalf("create bkt1: %v", err)
	}
	b2, err := s.CreateBucket(ctx, "bkt2", "alice", "STANDARD")
	if err != nil {
		t.Fatalf("create bkt2: %v", err)
	}

	for _, op := range bucketBlobOps() {
		t.Run(op.name, func(t *testing.T) {
			// Missing → ErrNoSuchX.
			if _, err := op.get(s, ctx, b1.ID); !errors.Is(err, op.missing) {
				t.Fatalf("get missing: got %v, want %v", err, op.missing)
			}
			// Set + Get round trip.
			payload := []byte("<" + op.name + ">cfg</" + op.name + ">")
			if err := op.set(s, ctx, b1.ID, payload); err != nil {
				t.Fatalf("set: %v", err)
			}
			got, err := op.get(s, ctx, b1.ID)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if string(got) != string(payload) {
				t.Fatalf("got %q want %q", got, payload)
			}
			// Per-bucket isolation: bkt2 has nothing.
			if _, err := op.get(s, ctx, b2.ID); !errors.Is(err, op.missing) {
				t.Fatalf("isolation: got %v, want %v", err, op.missing)
			}
			// Delete → Get returns missing again.
			if err := op.del(s, ctx, b1.ID); err != nil {
				t.Fatalf("delete: %v", err)
			}
			if _, err := op.get(s, ctx, b1.ID); !errors.Is(err, op.missing) {
				t.Fatalf("after delete: got %v, want %v", err, op.missing)
			}
			// Idempotent re-delete.
			if err := op.del(s, ctx, b1.ID); err != nil {
				t.Fatalf("re-delete: %v", err)
			}
		})
	}
}

func TestBucketBlobOverwrite(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	b, _ := s.CreateBucket(ctx, "bkt", "alice", "STANDARD")
	if err := s.SetBucketLifecycle(ctx, b.ID, []byte("v1")); err != nil {
		t.Fatalf("v1: %v", err)
	}
	if err := s.SetBucketLifecycle(ctx, b.ID, []byte("v2")); err != nil {
		t.Fatalf("v2: %v", err)
	}
	got, err := s.GetBucketLifecycle(ctx, b.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(got) != "v2" {
		t.Fatalf("got %q want v2", got)
	}
}

func TestBucketBlobKindsHaveDistinctKeys(t *testing.T) {
	// Cheap insurance against two BlobX constants accidentally aliasing onto
	// the same key — any clash would mean SetBucketCORS overwrites
	// SetBucketLifecycle, etc.
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	b, _ := s.CreateBucket(ctx, "bkt", "alice", "STANDARD")
	for i, op := range bucketBlobOps() {
		if err := op.set(s, ctx, b.ID, []byte(op.name)); err != nil {
			t.Fatalf("[%d %s] set: %v", i, op.name, err)
		}
	}
	for _, op := range bucketBlobOps() {
		got, err := op.get(s, ctx, b.ID)
		if err != nil {
			t.Fatalf("[%s] get: %v", op.name, err)
		}
		if string(got) != op.name {
			t.Fatalf("[%s] cross-talk: got %q want %q", op.name, got, op.name)
		}
	}
}

// ----------------------------------------------------------------------------
// US-007 — bucket inventory configs (per (bucket, configID)).

func TestInventoryConfigRoundTrip(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	b, _ := s.CreateBucket(ctx, "bkt", "alice", "STANDARD")

	// Missing → ErrNoSuchInventoryConfig.
	if _, err := s.GetBucketInventoryConfig(ctx, b.ID, "weekly"); !errors.Is(err, meta.ErrNoSuchInventoryConfig) {
		t.Fatalf("missing: %v want ErrNoSuchInventoryConfig", err)
	}

	// Set + Get round trip.
	if err := s.SetBucketInventoryConfig(ctx, b.ID, "weekly", []byte("<inv>weekly</inv>")); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, err := s.GetBucketInventoryConfig(ctx, b.ID, "weekly")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(got) != "<inv>weekly</inv>" {
		t.Fatalf("got %q", got)
	}

	// Overwrite.
	if err := s.SetBucketInventoryConfig(ctx, b.ID, "weekly", []byte("v2")); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	got, _ = s.GetBucketInventoryConfig(ctx, b.ID, "weekly")
	if string(got) != "v2" {
		t.Fatalf("after overwrite: %q", got)
	}

	// Delete + idempotent re-delete.
	if err := s.DeleteBucketInventoryConfig(ctx, b.ID, "weekly"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.GetBucketInventoryConfig(ctx, b.ID, "weekly"); !errors.Is(err, meta.ErrNoSuchInventoryConfig) {
		t.Fatalf("after delete: %v", err)
	}
	if err := s.DeleteBucketInventoryConfig(ctx, b.ID, "weekly"); err != nil {
		t.Fatalf("re-delete: %v", err)
	}
}

func TestInventoryConfigList(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	b, _ := s.CreateBucket(ctx, "bkt", "alice", "STANDARD")

	// Empty list returns an empty (non-nil) map.
	got, err := s.ListBucketInventoryConfigs(ctx, b.ID)
	if err != nil {
		t.Fatalf("empty list: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("empty len: %d", len(got))
	}

	configs := map[string]string{
		"daily":   "<a/>",
		"weekly":  "<b/>",
		"monthly": "<c/>",
		// Embed a 0x00 in the configID to exercise the byte-stuffing
		// codec. Decoders must round-trip it.
		"id\x00with\x00nul": "<d/>",
	}
	for id, blob := range configs {
		if err := s.SetBucketInventoryConfig(ctx, b.ID, id, []byte(blob)); err != nil {
			t.Fatalf("set %q: %v", id, err)
		}
	}

	got, err = s.ListBucketInventoryConfigs(ctx, b.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != len(configs) {
		t.Fatalf("len got=%d want=%d (%v)", len(got), len(configs), got)
	}
	for id, want := range configs {
		if string(got[id]) != want {
			t.Fatalf("[%q] got %q want %q", id, got[id], want)
		}
	}

	// Per-bucket isolation: a sibling bucket sees an empty list.
	other, _ := s.CreateBucket(ctx, "other", "alice", "STANDARD")
	if got, err := s.ListBucketInventoryConfigs(ctx, other.ID); err != nil || len(got) != 0 {
		t.Fatalf("other bucket: got %v len=%d err=%v", got, len(got), err)
	}

	// Names returned in ascending lex order would be a bonus, but the
	// contract is map-shaped — only assert the membership above.
	names := make([]string, 0, len(got))
	for k := range got {
		names = append(names, k)
	}
	sort.Strings(names)
	if names[0] == "" {
		t.Fatalf("empty configID surfaced: %v", names)
	}
}

// ----------------------------------------------------------------------------
// IAM (US-008).
// ----------------------------------------------------------------------------

func TestIAMUserCRUD(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	now := time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC)
	u := &meta.IAMUser{
		UserName:  "alice",
		UserID:    "AID-alice",
		Path:      "/team/eng/",
		CreatedAt: now,
	}
	if err := s.CreateIAMUser(ctx, u); err != nil {
		t.Fatalf("CreateIAMUser: %v", err)
	}

	got, err := s.GetIAMUser(ctx, "alice")
	if err != nil {
		t.Fatalf("GetIAMUser: %v", err)
	}
	if got.UserName != "alice" || got.UserID != "AID-alice" || got.Path != "/team/eng/" {
		t.Fatalf("user round-trip: got %+v", got)
	}
	if !got.CreatedAt.Equal(now) {
		t.Fatalf("CreatedAt: got %v want %v", got.CreatedAt, now)
	}
}

func TestIAMUserCreateDuplicate(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	if err := s.CreateIAMUser(ctx, &meta.IAMUser{UserName: "alice", UserID: "1"}); err != nil {
		t.Fatalf("first: %v", err)
	}
	err := s.CreateIAMUser(ctx, &meta.IAMUser{UserName: "alice", UserID: "2"})
	if !errors.Is(err, meta.ErrIAMUserAlreadyExists) {
		t.Fatalf("dup: got %v want ErrIAMUserAlreadyExists", err)
	}
}

func TestIAMUserGetMissing(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	_, err := s.GetIAMUser(ctx, "nope")
	if !errors.Is(err, meta.ErrIAMUserNotFound) {
		t.Fatalf("missing: got %v want ErrIAMUserNotFound", err)
	}
}

func TestIAMUserCreatedAtAutoFill(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	before := time.Now().UTC().Add(-time.Second)
	if err := s.CreateIAMUser(ctx, &meta.IAMUser{UserName: "alice", UserID: "1"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := s.GetIAMUser(ctx, "alice")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.CreatedAt.Before(before) || got.CreatedAt.After(time.Now().UTC().Add(time.Second)) {
		t.Fatalf("CreatedAt auto-fill out of band: %v", got.CreatedAt)
	}
}

func TestIAMUserDeleteHappyAndMissing(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	if err := s.CreateIAMUser(ctx, &meta.IAMUser{UserName: "alice", UserID: "1"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.DeleteIAMUser(ctx, "alice"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := s.DeleteIAMUser(ctx, "alice"); !errors.Is(err, meta.ErrIAMUserNotFound) {
		t.Fatalf("re-delete: got %v want ErrIAMUserNotFound", err)
	}
	if _, err := s.GetIAMUser(ctx, "alice"); !errors.Is(err, meta.ErrIAMUserNotFound) {
		t.Fatalf("get after delete: got %v want ErrIAMUserNotFound", err)
	}
}

func TestIAMUserListOrderingAndPathFilter(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	users := []*meta.IAMUser{
		{UserName: "carol", UserID: "3", Path: "/team/ops/"},
		{UserName: "alice", UserID: "1", Path: "/team/eng/"},
		{UserName: "bob", UserID: "2", Path: "/team/eng/"},
		{UserName: "dave", UserID: "4", Path: "/external/"},
	}
	for _, u := range users {
		if err := s.CreateIAMUser(ctx, u); err != nil {
			t.Fatalf("create %q: %v", u.UserName, err)
		}
	}

	all, err := s.ListIAMUsers(ctx, "")
	if err != nil {
		t.Fatalf("ListIAMUsers: %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("len got=%d want=4", len(all))
	}
	wantOrder := []string{"alice", "bob", "carol", "dave"}
	for i, want := range wantOrder {
		if all[i].UserName != want {
			t.Fatalf("order[%d]: got %q want %q (full: %v)", i, all[i].UserName, want, wantOrder)
		}
	}

	eng, err := s.ListIAMUsers(ctx, "/team/eng/")
	if err != nil {
		t.Fatalf("path filter: %v", err)
	}
	if len(eng) != 2 || eng[0].UserName != "alice" || eng[1].UserName != "bob" {
		t.Fatalf("path filter result: %v", eng)
	}

	if got, err := s.ListIAMUsers(ctx, "/nonexistent/"); err != nil || len(got) != 0 {
		t.Fatalf("no-match filter: got %v err=%v", got, err)
	}
}

func TestIAMAccessKeyCRUD(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	now := time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC)
	ak := &meta.IAMAccessKey{
		AccessKeyID:     "AKIA-TEST",
		SecretAccessKey: "shhh",
		UserName:        "alice",
		CreatedAt:       now,
		Disabled:        false,
	}
	if err := s.CreateIAMAccessKey(ctx, ak); err != nil {
		t.Fatalf("CreateIAMAccessKey: %v", err)
	}

	got, err := s.GetIAMAccessKey(ctx, "AKIA-TEST")
	if err != nil {
		t.Fatalf("GetIAMAccessKey: %v", err)
	}
	if got.AccessKeyID != ak.AccessKeyID || got.SecretAccessKey != ak.SecretAccessKey ||
		got.UserName != ak.UserName || !got.CreatedAt.Equal(now) {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}

func TestIAMAccessKeyGetMissing(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	_, err := s.GetIAMAccessKey(ctx, "AKIA-NOPE")
	if !errors.Is(err, meta.ErrIAMAccessKeyNotFound) {
		t.Fatalf("missing: got %v want ErrIAMAccessKeyNotFound", err)
	}
}

func TestIAMAccessKeyDisabledFlag(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	if err := s.CreateIAMAccessKey(ctx, &meta.IAMAccessKey{
		AccessKeyID: "AKIA-DOWN", SecretAccessKey: "x", UserName: "alice", Disabled: true,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := s.GetIAMAccessKey(ctx, "AKIA-DOWN")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.Disabled {
		t.Fatalf("Disabled flag lost: %+v", got)
	}
}

func TestIAMAccessKeyListByUser(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	keys := []*meta.IAMAccessKey{
		{AccessKeyID: "AKIA-A2", SecretAccessKey: "s", UserName: "alice"},
		{AccessKeyID: "AKIA-A1", SecretAccessKey: "s", UserName: "alice"},
		{AccessKeyID: "AKIA-B1", SecretAccessKey: "s", UserName: "bob"},
	}
	for _, ak := range keys {
		if err := s.CreateIAMAccessKey(ctx, ak); err != nil {
			t.Fatalf("create %q: %v", ak.AccessKeyID, err)
		}
	}

	alice, err := s.ListIAMAccessKeys(ctx, "alice")
	if err != nil {
		t.Fatalf("list alice: %v", err)
	}
	if len(alice) != 2 {
		t.Fatalf("alice len: got %d want 2 (%v)", len(alice), alice)
	}
	// Index range scan returns access-key IDs in ascending lex order.
	if alice[0].AccessKeyID != "AKIA-A1" || alice[1].AccessKeyID != "AKIA-A2" {
		t.Fatalf("alice order: %v", alice)
	}

	bob, err := s.ListIAMAccessKeys(ctx, "bob")
	if err != nil {
		t.Fatalf("list bob: %v", err)
	}
	if len(bob) != 1 || bob[0].AccessKeyID != "AKIA-B1" {
		t.Fatalf("bob: %v", bob)
	}

	if got, err := s.ListIAMAccessKeys(ctx, "carol"); err != nil || len(got) != 0 {
		t.Fatalf("unknown user: got %v err=%v", got, err)
	}
}

func TestIAMAccessKeyListNoUserFilter(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	for _, ak := range []*meta.IAMAccessKey{
		{AccessKeyID: "AKIA-Z", SecretAccessKey: "s", UserName: "alice"},
		{AccessKeyID: "AKIA-A", SecretAccessKey: "s", UserName: "bob"},
		{AccessKeyID: "AKIA-M", SecretAccessKey: "s", UserName: "carol"},
	} {
		if err := s.CreateIAMAccessKey(ctx, ak); err != nil {
			t.Fatalf("create %q: %v", ak.AccessKeyID, err)
		}
	}

	all, err := s.ListIAMAccessKeys(ctx, "")
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("len: %d want 3", len(all))
	}
	wantOrder := []string{"AKIA-A", "AKIA-M", "AKIA-Z"}
	for i, want := range wantOrder {
		if all[i].AccessKeyID != want {
			t.Fatalf("order[%d]: got %q want %q", i, all[i].AccessKeyID, want)
		}
	}
}

func TestIAMAccessKeyDeleteHappyAndMissing(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	ak := &meta.IAMAccessKey{
		AccessKeyID: "AKIA-X", SecretAccessKey: "shh", UserName: "alice",
	}
	if err := s.CreateIAMAccessKey(ctx, ak); err != nil {
		t.Fatalf("create: %v", err)
	}
	deleted, err := s.DeleteIAMAccessKey(ctx, "AKIA-X")
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if deleted == nil || deleted.AccessKeyID != "AKIA-X" || deleted.UserName != "alice" {
		t.Fatalf("deleted record: %+v", deleted)
	}

	if _, err := s.GetIAMAccessKey(ctx, "AKIA-X"); !errors.Is(err, meta.ErrIAMAccessKeyNotFound) {
		t.Fatalf("get after delete: got %v want ErrIAMAccessKeyNotFound", err)
	}

	// Index row is also gone — list by user returns empty.
	if got, err := s.ListIAMAccessKeys(ctx, "alice"); err != nil || len(got) != 0 {
		t.Fatalf("list after delete: %v err=%v", got, err)
	}

	// Re-delete is ErrIAMAccessKeyNotFound.
	if _, err := s.DeleteIAMAccessKey(ctx, "AKIA-X"); !errors.Is(err, meta.ErrIAMAccessKeyNotFound) {
		t.Fatalf("re-delete: got %v want ErrIAMAccessKeyNotFound", err)
	}
}

// TestIAMAccessKeyIndexCleanup proves the secondary (per-user) index
// stays in sync with the per-key record after a delete: a fresh user
// added at the same time has its access key surface in ListIAMAccessKeys
// even after a sibling delete clears the older entries.
func TestIAMAccessKeyIndexCleanup(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	for _, ak := range []*meta.IAMAccessKey{
		{AccessKeyID: "AKIA-OLD", SecretAccessKey: "x", UserName: "alice"},
		{AccessKeyID: "AKIA-KEEP", SecretAccessKey: "x", UserName: "alice"},
	} {
		if err := s.CreateIAMAccessKey(ctx, ak); err != nil {
			t.Fatalf("create %q: %v", ak.AccessKeyID, err)
		}
	}
	if _, err := s.DeleteIAMAccessKey(ctx, "AKIA-OLD"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	got, err := s.ListIAMAccessKeys(ctx, "alice")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].AccessKeyID != "AKIA-KEEP" {
		t.Fatalf("after delete: %v", got)
	}
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

// ----------------------------------------------------------------------------
// Audit log (US-009).
//
// EnqueueAudit / ListAudit / ListAuditFiltered / ListAuditPartitionsBefore /
// ReadAuditPartition / DeleteAuditPartition + sweeper.

func auditUUID(t *testing.T, s string) uuid.UUID {
	t.Helper()
	id, err := uuid.Parse(s)
	if err != nil {
		t.Fatalf("uuid.Parse(%q): %v", s, err)
	}
	return id
}

func TestEnqueueAuditAndListByBucket(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	bid := auditUUID(t, "11111111-2222-3333-4444-555555555555")
	now := time.Date(2026, 4, 29, 10, 0, 0, 0, time.UTC)

	for i := 0; i < 3; i++ {
		evt := &meta.AuditEvent{
			BucketID:  bid,
			Bucket:    "bkt",
			Time:      now.Add(time.Duration(i) * time.Minute),
			Principal: "alice",
			Action:    "PutObject",
			Resource:  "s3:/bkt/k",
			Result:    "success",
			RequestID: gocql.TimeUUID().String(),
			SourceIP:  "127.0.0.1",
		}
		if err := s.EnqueueAudit(ctx, evt, 0); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
		if evt.EventID == "" {
			t.Fatalf("EnqueueAudit did not auto-fill EventID")
		}
	}

	got, err := s.ListAudit(ctx, bid, 100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len: got %d want 3", len(got))
	}
	// Newest-first ordering.
	for i := 0; i < len(got)-1; i++ {
		if got[i].Time.Before(got[i+1].Time) {
			t.Fatalf("not newest-first at %d: %v then %v", i, got[i].Time, got[i+1].Time)
		}
	}
}

func TestEnqueueAuditDefaultsAndIAMBucket(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	evt := &meta.AuditEvent{
		BucketID:  uuid.Nil,
		Action:    "iam:CreateUser",
		Resource:  "iam:CreateUser",
		Principal: "root",
		Result:    "success",
	}
	if err := s.EnqueueAudit(ctx, evt, 0); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if evt.Time.IsZero() {
		t.Fatalf("Time auto-fill missing")
	}
	if evt.EventID == "" {
		t.Fatalf("EventID auto-fill missing")
	}
	if evt.Bucket != "-" {
		t.Fatalf("IAM rows should default Bucket to '-': %q", evt.Bucket)
	}
}

func TestEnqueueAuditNilNoop(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	if err := s.EnqueueAudit(context.Background(), nil, 0); err != nil {
		t.Fatalf("nil entry should be a no-op, got %v", err)
	}
}

func TestListAuditFilteredBucketScopedAndPrincipal(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	bid := auditUUID(t, "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	other := auditUUID(t, "ffffffff-1111-2222-3333-444444444444")
	now := time.Now().UTC()

	mk := func(b uuid.UUID, name, principal string, off time.Duration) {
		err := s.EnqueueAudit(ctx, &meta.AuditEvent{
			BucketID:  b,
			Bucket:    name,
			Time:      now.Add(off),
			Principal: principal,
			Action:    "PutObject",
		}, 0)
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}
	mk(bid, "bkt", "alice", -2*time.Hour)
	mk(bid, "bkt", "bob", -time.Hour)
	mk(bid, "bkt", "alice", -30*time.Minute)
	mk(other, "other", "alice", -15*time.Minute)

	rows, _, err := s.ListAuditFiltered(ctx, meta.AuditFilter{
		BucketID:     bid,
		BucketScoped: true,
		Limit:        10,
	})
	if err != nil {
		t.Fatalf("filtered: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("bucket-scoped: got %d want 3", len(rows))
	}

	rows, _, err = s.ListAuditFiltered(ctx, meta.AuditFilter{
		BucketID:     bid,
		BucketScoped: true,
		Principal:    "alice",
		Limit:        10,
	})
	if err != nil {
		t.Fatalf("principal-filtered: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("principal=alice: got %d want 2", len(rows))
	}
	for _, r := range rows {
		if r.Principal != "alice" {
			t.Fatalf("principal filter leaked: %+v", r)
		}
	}

	rows, _, err = s.ListAuditFiltered(ctx, meta.AuditFilter{Limit: 10})
	if err != nil {
		t.Fatalf("global: %v", err)
	}
	if len(rows) != 4 {
		t.Fatalf("global: got %d want 4", len(rows))
	}
}

func TestListAuditFilteredPagination(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	bid := auditUUID(t, "12121212-3434-5656-7878-909090909090")
	now := time.Now().UTC()

	for i := 0; i < 5; i++ {
		err := s.EnqueueAudit(ctx, &meta.AuditEvent{
			BucketID: bid,
			Bucket:   "bkt",
			Time:     now.Add(-time.Duration(i) * time.Minute),
			Action:   "PutObject",
		}, 0)
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}
	page1, next, err := s.ListAuditFiltered(ctx, meta.AuditFilter{
		BucketID:     bid,
		BucketScoped: true,
		Limit:        2,
	})
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if len(page1) != 2 {
		t.Fatalf("page1 len: %d", len(page1))
	}
	if next == "" {
		t.Fatalf("page1 next empty")
	}
	page2, next2, err := s.ListAuditFiltered(ctx, meta.AuditFilter{
		BucketID:     bid,
		BucketScoped: true,
		Limit:        2,
		Continuation: next,
	})
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	if len(page2) != 2 {
		t.Fatalf("page2 len: %d", len(page2))
	}
	if next2 == "" {
		t.Fatalf("page2 next empty")
	}
	page3, next3, err := s.ListAuditFiltered(ctx, meta.AuditFilter{
		BucketID:     bid,
		BucketScoped: true,
		Limit:        2,
		Continuation: next2,
	})
	if err != nil {
		t.Fatalf("page3: %v", err)
	}
	if len(page3) != 1 {
		t.Fatalf("page3 len: %d", len(page3))
	}
	if next3 != "" {
		t.Fatalf("page3 next should be empty, got %q", next3)
	}
}

func TestAuditPartitionsAndReadDelete(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	bid := auditUUID(t, "deadbeef-feed-cafe-babe-c0ffeec0ffee")
	now := time.Now().UTC()
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	older := day.AddDate(0, 0, -10)

	mk := func(at time.Time, action string) {
		err := s.EnqueueAudit(ctx, &meta.AuditEvent{
			BucketID: bid,
			Bucket:   "bkt",
			Time:     at,
			Action:   action,
		}, 0)
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}
	mk(older.Add(time.Hour), "GetObject")
	mk(older.Add(2*time.Hour), "PutObject")
	mk(now, "DeleteObject")

	parts, err := s.ListAuditPartitionsBefore(ctx, day)
	if err != nil {
		t.Fatalf("partitions before: %v", err)
	}
	if len(parts) != 1 {
		t.Fatalf("partitions: got %d want 1; %+v", len(parts), parts)
	}
	if !parts[0].Day.Equal(older) {
		t.Fatalf("partition day: got %v want %v", parts[0].Day, older)
	}
	if parts[0].BucketID != bid || parts[0].Bucket != "bkt" {
		t.Fatalf("partition bucket: %+v", parts[0])
	}

	rows, err := s.ReadAuditPartition(ctx, bid, older)
	if err != nil {
		t.Fatalf("read partition: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("partition rows: got %d want 2", len(rows))
	}
	// Sorted ascending by EventID.
	for i := 0; i < len(rows)-1; i++ {
		if rows[i].EventID > rows[i+1].EventID {
			t.Fatalf("partition rows not eventID-asc")
		}
	}

	if err := s.DeleteAuditPartition(ctx, bid, older); err != nil {
		t.Fatalf("delete partition: %v", err)
	}
	rows, err = s.ReadAuditPartition(ctx, bid, older)
	if err != nil {
		t.Fatalf("read after delete: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("partition not drained: got %d rows", len(rows))
	}
	// Today's row survived.
	got, err := s.ListAudit(ctx, bid, 10)
	if err != nil {
		t.Fatalf("list after partition delete: %v", err)
	}
	if len(got) != 1 || got[0].Action != "DeleteObject" {
		t.Fatalf("today row missing: %+v", got)
	}
}

func TestEnqueueAuditTTLLazyExpiry(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	bid := auditUUID(t, "11111111-1111-1111-1111-111111111111")

	// 1ns TTL → row expires immediately. ListAudit lazy-skips it.
	err := s.EnqueueAudit(ctx, &meta.AuditEvent{
		BucketID: bid,
		Bucket:   "bkt",
		Time:     time.Now().UTC(),
		Action:   "PutObject",
	}, time.Nanosecond)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	// Sleep a bit so wall clock passes the 1ns ExpiresAt.
	time.Sleep(2 * time.Millisecond)

	rows, err := s.ListAudit(ctx, bid, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("expected 0 rows after ttl lapse, got %d", len(rows))
	}

	// A long TTL row should still surface.
	err = s.EnqueueAudit(ctx, &meta.AuditEvent{
		BucketID: bid,
		Bucket:   "bkt",
		Time:     time.Now().UTC(),
		Action:   "GetObject",
	}, 10*time.Minute)
	if err != nil {
		t.Fatalf("enqueue keep: %v", err)
	}
	rows, err = s.ListAudit(ctx, bid, 10)
	if err != nil {
		t.Fatalf("list keep: %v", err)
	}
	if len(rows) != 1 || rows[0].Action != "GetObject" {
		t.Fatalf("kept row missing: %+v", rows)
	}
}

func TestAuditSweeperRunOnceDeletesExpired(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	bid := auditUUID(t, "22222222-2222-2222-2222-222222222222")

	// Two rows: one with a tiny TTL (will be expired), one untouched.
	err := s.EnqueueAudit(ctx, &meta.AuditEvent{
		BucketID: bid,
		Bucket:   "bkt",
		Time:     time.Now().UTC(),
		Action:   "PutObject",
	}, time.Nanosecond)
	if err != nil {
		t.Fatalf("expired enqueue: %v", err)
	}
	err = s.EnqueueAudit(ctx, &meta.AuditEvent{
		BucketID: bid,
		Bucket:   "bkt",
		Time:     time.Now().UTC(),
		Action:   "GetObject",
	}, time.Hour)
	if err != nil {
		t.Fatalf("kept enqueue: %v", err)
	}

	time.Sleep(2 * time.Millisecond)

	sw, err := NewAuditSweeper(AuditSweeperConfig{
		Store:    s,
		Locker:   newDummyLocker(),
		Interval: time.Hour,
	})
	if err != nil {
		t.Fatalf("new sweeper: %v", err)
	}
	deleted, err := sw.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("deleted: got %d want 1", deleted)
	}

	// The kept row is still readable; the expired row is gone (re-running
	// the sweep deletes nothing further).
	rows, err := s.ListAudit(ctx, bid, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 || rows[0].Action != "GetObject" {
		t.Fatalf("kept row missing post-sweep: %+v", rows)
	}
	deleted2, err := sw.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce idempotent: %v", err)
	}
	if deleted2 != 0 {
		t.Fatalf("idempotent delete count: %d", deleted2)
	}
}

func TestAuditSweeperLeaderElection(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bid := auditUUID(t, "33333333-3333-3333-3333-333333333333")

	if err := s.EnqueueAudit(ctx, &meta.AuditEvent{
		BucketID: bid,
		Bucket:   "bkt",
		Time:     time.Now().UTC(),
		Action:   "PutObject",
	}, time.Nanosecond); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	time.Sleep(2 * time.Millisecond)

	locker := newDummyLocker()
	sw, err := NewAuditSweeper(AuditSweeperConfig{
		Store:    s,
		Locker:   locker,
		Interval: 50 * time.Millisecond,
		Holder:   "test",
	})
	if err != nil {
		t.Fatalf("new sweeper: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- sw.Run(ctx) }()

	// Wait for the immediate tick to drain the expired row.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rows, err := s.ListAudit(ctx, bid, 10)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(rows) == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("sweeper exit: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatalf("sweeper did not exit on ctx cancel")
	}
}

