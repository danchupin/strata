// Package storetest ships a shared contract test that any meta.Store
// implementation must pass. The memory store runs this suite unconditionally;
// Cassandra runs it under -tags integration.
package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/meta"
)

// Run exercises the full meta.Store surface against the given factory.
// The factory must return a fresh, empty store; it is called once per test.
func Run(t *testing.T, newStore func(t *testing.T) meta.Store) {
	t.Helper()
	cases := []struct {
		name string
		fn   func(t *testing.T, s meta.Store)
	}{
		{"BucketLifecycle", caseBucketLifecycle},
		{"VersionedObjectOverwrite", caseVersionedOverwrite},
		{"DeleteMarkerHidesObject", caseDeleteMarker},
		{"SetObjectStorageCAS", caseSetObjectStorageCAS},
		{"GCQueueRoundTrip", caseGCQueueRoundTrip},
		{"BucketLifecycleRulesBlob", caseLifecycleBlob},
		{"ListObjectsHidesDeleteMarkers", caseListHidesDeleteMarkers},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.fn(t, newStore(t))
		})
	}
}

func caseBucketLifecycle(t *testing.T, s meta.Store) {
	ctx := context.Background()
	b, err := s.CreateBucket(ctx, "foo", "owner-a", "STANDARD")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if b.Name != "foo" || b.Versioning != meta.VersioningDisabled {
		t.Fatalf("create result: %+v", b)
	}
	if _, err := s.CreateBucket(ctx, "foo", "owner-a", "STANDARD"); err != meta.ErrBucketAlreadyExists {
		t.Errorf("duplicate: got %v want ErrBucketAlreadyExists", err)
	}
	got, err := s.GetBucket(ctx, "foo")
	if err != nil || got.ID != b.ID {
		t.Fatalf("get: %v %+v", err, got)
	}
	if err := s.SetBucketVersioning(ctx, "foo", meta.VersioningEnabled); err != nil {
		t.Fatalf("set versioning: %v", err)
	}
	got, _ = s.GetBucket(ctx, "foo")
	if got.Versioning != meta.VersioningEnabled {
		t.Errorf("versioning after set: %s", got.Versioning)
	}
	if err := s.DeleteBucket(ctx, "foo"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.GetBucket(ctx, "foo"); err != meta.ErrBucketNotFound {
		t.Errorf("after delete: %v", err)
	}
}

func caseVersionedOverwrite(t *testing.T, s meta.Store) {
	ctx := context.Background()
	b, _ := s.CreateBucket(ctx, "v", "o", "STANDARD")
	_ = s.SetBucketVersioning(ctx, "v", meta.VersioningEnabled)
	b, _ = s.GetBucket(ctx, "v")

	putOne := func(body string) string {
		o := &meta.Object{
			BucketID:     b.ID,
			Key:          "doc",
			StorageClass: "STANDARD",
			ETag:         body,
			Mtime:        time.Now().UTC(),
			Manifest:     &data.Manifest{Class: "STANDARD", Size: int64(len(body))},
			Size:         int64(len(body)),
		}
		if err := s.PutObject(ctx, o, true); err != nil {
			t.Fatal(err)
		}
		return o.VersionID
	}
	v1 := putOne("one")
	v2 := putOne("two")
	v3 := putOne("three")
	if v1 == v2 || v2 == v3 {
		t.Fatalf("version ids not distinct: %s %s %s", v1, v2, v3)
	}
	latest, err := s.GetObject(ctx, b.ID, "doc", "")
	if err != nil {
		t.Fatal(err)
	}
	if latest.VersionID != v3 {
		t.Errorf("latest should be v3, got %s (want %s)", latest.VersionID, v3)
	}
	old, err := s.GetObject(ctx, b.ID, "doc", v1)
	if err != nil || old.ETag != "one" {
		t.Errorf("v1 lookup: %v %+v", err, old)
	}
}

func caseDeleteMarker(t *testing.T, s meta.Store) {
	ctx := context.Background()
	b, _ := s.CreateBucket(ctx, "d", "o", "STANDARD")
	_ = s.SetBucketVersioning(ctx, "d", meta.VersioningEnabled)
	b, _ = s.GetBucket(ctx, "d")
	o := &meta.Object{
		BucketID:     b.ID,
		Key:          "k",
		StorageClass: "STANDARD",
		ETag:         "x",
		Size:         1,
		Mtime:        time.Now().UTC(),
		Manifest:     &data.Manifest{Class: "STANDARD"},
	}
	if err := s.PutObject(ctx, o, true); err != nil {
		t.Fatal(err)
	}
	origVersion := o.VersionID

	dm, err := s.DeleteObject(ctx, b.ID, "k", "", true)
	if err != nil || dm == nil || !dm.IsDeleteMarker {
		t.Fatalf("delete marker: %v %+v", err, dm)
	}
	if _, err := s.GetObject(ctx, b.ID, "k", ""); err != meta.ErrObjectNotFound {
		t.Errorf("after delete marker: %v want ErrObjectNotFound", err)
	}
	got, err := s.GetObject(ctx, b.ID, "k", origVersion)
	if err != nil || got.ETag != "x" {
		t.Errorf("original version should still be readable: %v %+v", err, got)
	}
}

func caseSetObjectStorageCAS(t *testing.T, s meta.Store) {
	ctx := context.Background()
	b, _ := s.CreateBucket(ctx, "cas", "o", "STANDARD")
	o := &meta.Object{
		BucketID: b.ID, Key: "k",
		StorageClass: "STANDARD", ETag: "e", Size: 1,
		Mtime:    time.Now().UTC(),
		Manifest: &data.Manifest{Class: "STANDARD"},
	}
	if err := s.PutObject(ctx, o, false); err != nil {
		t.Fatal(err)
	}
	newMan := &data.Manifest{Class: "STANDARD_IA"}
	applied, err := s.SetObjectStorage(ctx, b.ID, "k", o.VersionID, "STANDARD", "STANDARD_IA", newMan)
	if err != nil || !applied {
		t.Fatalf("CAS should apply when expectedClass matches: applied=%v err=%v", applied, err)
	}

	applied, err = s.SetObjectStorage(ctx, b.ID, "k", o.VersionID, "STANDARD", "GLACIER_IR", newMan)
	if err != nil {
		t.Fatalf("CAS err: %v", err)
	}
	if applied {
		t.Error("CAS should have rejected because class already changed to STANDARD_IA")
	}
}

func caseGCQueueRoundTrip(t *testing.T, s meta.Store) {
	ctx := context.Background()
	chunks := []data.ChunkRef{
		{Cluster: "c", Pool: "p1", OID: "o1", Size: 100},
		{Cluster: "c", Pool: "p2", OID: "o2", Size: 200},
	}
	if err := s.EnqueueChunkDeletion(ctx, "default", chunks); err != nil {
		t.Fatal(err)
	}
	entries, err := s.ListGCEntries(ctx, "default", time.Now().Add(time.Hour), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries want 2", len(entries))
	}
	for _, e := range entries {
		if err := s.AckGCEntry(ctx, "default", e); err != nil {
			t.Fatal(err)
		}
	}
	remaining, _ := s.ListGCEntries(ctx, "default", time.Now().Add(time.Hour), 100)
	if len(remaining) != 0 {
		t.Errorf("after ack: %d remaining", len(remaining))
	}
}

func caseLifecycleBlob(t *testing.T, s meta.Store) {
	ctx := context.Background()
	b, _ := s.CreateBucket(ctx, "lc", "o", "STANDARD")
	if _, err := s.GetBucketLifecycle(ctx, b.ID); err != meta.ErrNoSuchLifecycle {
		t.Errorf("initial: got %v want ErrNoSuchLifecycle", err)
	}
	payload := []byte(`<LifecycleConfiguration><Rule><ID>x</ID></Rule></LifecycleConfiguration>`)
	if err := s.SetBucketLifecycle(ctx, b.ID, payload); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetBucketLifecycle(ctx, b.ID)
	if err != nil || string(got) != string(payload) {
		t.Errorf("roundtrip: %v %q", err, got)
	}
	if err := s.DeleteBucketLifecycle(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetBucketLifecycle(ctx, b.ID); err != meta.ErrNoSuchLifecycle {
		t.Errorf("after delete: got %v want ErrNoSuchLifecycle", err)
	}
}

func caseListHidesDeleteMarkers(t *testing.T, s meta.Store) {
	ctx := context.Background()
	b, _ := s.CreateBucket(ctx, "lst", "o", "STANDARD")
	_ = s.SetBucketVersioning(ctx, "lst", meta.VersioningEnabled)
	b, _ = s.GetBucket(ctx, "lst")

	put := func(key, body string) {
		o := &meta.Object{
			BucketID:     b.ID,
			Key:          key,
			StorageClass: "STANDARD",
			ETag:         body,
			Size:         int64(len(body)),
			Mtime:        time.Now().UTC(),
			Manifest:     &data.Manifest{Class: "STANDARD"},
		}
		if err := s.PutObject(ctx, o, true); err != nil {
			t.Fatal(err)
		}
	}
	put("a", "1")
	put("b", "1")
	put("c", "1")
	if _, err := s.DeleteObject(ctx, b.ID, "b", "", true); err != nil {
		t.Fatal(err)
	}

	list, err := s.ListObjects(ctx, b.ID, meta.ListOptions{Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, o := range list.Objects {
		keys = append(keys, o.Key)
	}
	if len(keys) != 2 || keys[0] != "a" || keys[1] != "c" {
		t.Errorf("list: %v (expected [a c])", keys)
	}

	versions, err := s.ListObjectVersions(ctx, b.ID, meta.ListOptions{Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	var seenDM bool
	for _, v := range versions.Versions {
		if v.IsDeleteMarker {
			seenDM = true
		}
	}
	if !seenDM {
		t.Errorf("ListObjectVersions should include delete markers")
	}
}
