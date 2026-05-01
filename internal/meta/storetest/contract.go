// Package storetest ships a shared contract test that any meta.Store
// implementation must pass. The memory store runs this suite unconditionally;
// Cassandra runs it under -tags integration.
package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/gocql/gocql"

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
		{"ListObjectsDelimiterPrefixPaging", caseListDelimiterPrefixPaging},
		{"CompleteMultipartPopulatesPartChunks", caseCompleteMultipartPopulatesPartChunks},
		{"NullVersionLifecycle", caseNullVersionLifecycle},
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

func caseCompleteMultipartPopulatesPartChunks(t *testing.T, s meta.Store) {
	ctx := context.Background()
	b, _ := s.CreateBucket(ctx, "mp", "o", "STANDARD")

	mu := &meta.MultipartUpload{
		BucketID:     b.ID,
		UploadID:     gocql.TimeUUID().String(),
		Key:          "obj",
		StorageClass: "STANDARD",
		ContentType:  "application/octet-stream",
		InitiatedAt:  time.Now().UTC(),
		Status:       "uploading",
	}
	if err := s.CreateMultipartUpload(ctx, mu); err != nil {
		t.Fatalf("create mp: %v", err)
	}

	parts := []*meta.MultipartPart{
		{PartNumber: 1, ETag: "aa", Size: 5 * 1024 * 1024, Manifest: &data.Manifest{Class: "STANDARD"}},
		{PartNumber: 2, ETag: "bb", Size: 5 * 1024 * 1024, Manifest: &data.Manifest{Class: "STANDARD"}},
		{PartNumber: 3, ETag: "cc", Size: 1024, Manifest: &data.Manifest{Class: "STANDARD"}},
	}
	for _, p := range parts {
		if err := s.SavePart(ctx, b.ID, mu.UploadID, p); err != nil {
			t.Fatalf("save part %d: %v", p.PartNumber, err)
		}
	}

	obj := &meta.Object{
		BucketID:     b.ID,
		Key:          "obj",
		ContentType:  "application/octet-stream",
		StorageClass: "STANDARD",
		ETag:         "composite",
		Mtime:        time.Now().UTC(),
	}
	complete := []meta.CompletePart{
		{PartNumber: 1, ETag: "aa"},
		{PartNumber: 2, ETag: "bb"},
		{PartNumber: 3, ETag: "cc"},
	}
	if _, err := s.CompleteMultipartUpload(ctx, obj, mu.UploadID, complete, false); err != nil {
		t.Fatalf("complete: %v", err)
	}

	got, err := s.GetObject(ctx, b.ID, "obj", "")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Manifest == nil {
		t.Fatal("manifest nil after complete")
	}
	if len(got.Manifest.PartChunks) != 3 {
		t.Fatalf("PartChunks len: got %d want 3 (%+v)", len(got.Manifest.PartChunks), got.Manifest.PartChunks)
	}
	want := []data.PartRange{
		{PartNumber: 1, Offset: 0, Size: 5 * 1024 * 1024, ETag: "aa"},
		{PartNumber: 2, Offset: 5 * 1024 * 1024, Size: 5 * 1024 * 1024, ETag: "bb"},
		{PartNumber: 3, Offset: 10 * 1024 * 1024, Size: 1024, ETag: "cc"},
	}
	for i, w := range want {
		g := got.Manifest.PartChunks[i]
		if g.PartNumber != w.PartNumber || g.Offset != w.Offset || g.Size != w.Size || g.ETag != w.ETag {
			t.Errorf("PartChunks[%d]: got %+v want %+v", i, g, w)
		}
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

// caseNullVersionLifecycle exercises US-007: literal-"null" version-id
// stored on PUT-to-unversioned bucket survives Enabled+overwrite, gets
// replaced atomically on Suspended PUT, and surfaces in GET / DELETE /
// ListObjectVersions alongside UUID-versioned rows.
func caseNullVersionLifecycle(t *testing.T, s meta.Store) {
	ctx := context.Background()
	b, _ := s.CreateBucket(ctx, "nv", "o", "STANDARD")

	put := func(body, versionID string, versioned bool) *meta.Object {
		o := &meta.Object{
			BucketID:     b.ID,
			Key:          "k",
			VersionID:    versionID,
			StorageClass: "STANDARD",
			ETag:         body,
			Size:         int64(len(body)),
			Mtime:        time.Now().UTC(),
			Manifest:     &data.Manifest{Class: "STANDARD"},
		}
		if err := s.PutObject(ctx, o, versioned); err != nil {
			t.Fatalf("put %q: %v", body, err)
		}
		return o
	}

	// 1. PUT to unversioned (Disabled) bucket — null version.
	o1 := put("v1-disabled", meta.NullVersionID, false)
	if o1.VersionID != meta.NullVersionID {
		t.Fatalf("disabled put VersionID: got %q want %q", o1.VersionID, meta.NullVersionID)
	}

	// 2. ?versionId=null resolves on Disabled bucket.
	got, err := s.GetObject(ctx, b.ID, "k", meta.NullVersionID)
	if err != nil || got.ETag != "v1-disabled" {
		t.Fatalf("disabled GET ?versionId=null: %v %+v", err, got)
	}

	// 3. Enable + overwrite — prior null preserved.
	_ = s.SetBucketVersioning(ctx, "nv", meta.VersioningEnabled)
	o2 := put("v2-uuid", "", true)
	if o2.VersionID == meta.NullVersionID || o2.VersionID == "" {
		t.Fatalf("enabled put VersionID: got %q (want UUID)", o2.VersionID)
	}
	got, err = s.GetObject(ctx, b.ID, "k", "")
	if err != nil || got.ETag != "v2-uuid" {
		t.Fatalf("latest after enable: %v %+v", err, got)
	}
	got, err = s.GetObject(ctx, b.ID, "k", meta.NullVersionID)
	if err != nil || got.ETag != "v1-disabled" {
		t.Fatalf("null version still readable: %v %+v", err, got)
	}

	// 4. Suspend + overwrite — null replaced atomically, UUID preserved.
	_ = s.SetBucketVersioning(ctx, "nv", meta.VersioningSuspended)
	o3 := put("v3-suspended", meta.NullVersionID, true)
	if o3.VersionID != meta.NullVersionID {
		t.Fatalf("suspended put VersionID: got %q want null", o3.VersionID)
	}
	got, err = s.GetObject(ctx, b.ID, "k", meta.NullVersionID)
	if err != nil || got.ETag != "v3-suspended" {
		t.Fatalf("null after suspend overwrite: %v %+v", err, got)
	}
	got, err = s.GetObject(ctx, b.ID, "k", o2.VersionID)
	if err != nil || got.ETag != "v2-uuid" {
		t.Fatalf("UUID version preserved through suspend: %v %+v", err, got)
	}

	// 5. ListObjectVersions surfaces both null + UUID.
	lst, err := s.ListObjectVersions(ctx, b.ID, meta.ListOptions{Limit: 1000})
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	var sawNull, sawUUID bool
	for _, v := range lst.Versions {
		switch v.VersionID {
		case meta.NullVersionID:
			sawNull = true
			if v.ETag != "v3-suspended" {
				t.Errorf("null row ETag: got %q want v3-suspended", v.ETag)
			}
		case o2.VersionID:
			sawUUID = true
		}
	}
	if !sawNull || !sawUUID {
		t.Errorf("list missing rows (null=%v uuid=%v): %+v", sawNull, sawUUID, lst.Versions)
	}

	// 6. DELETE ?versionId=null targets the null row alone.
	if _, err := s.DeleteObject(ctx, b.ID, "k", meta.NullVersionID, true); err != nil {
		t.Fatalf("delete null: %v", err)
	}
	if _, err := s.GetObject(ctx, b.ID, "k", meta.NullVersionID); err != meta.ErrObjectNotFound {
		t.Errorf("after delete null: got %v want ErrObjectNotFound", err)
	}
	got, err = s.GetObject(ctx, b.ID, "k", o2.VersionID)
	if err != nil || got.ETag != "v2-uuid" {
		t.Errorf("UUID survives null delete: %v %+v", err, got)
	}
}

func caseListDelimiterPrefixPaging(t *testing.T, s meta.Store) {
	ctx := context.Background()
	b, _ := s.CreateBucket(ctx, "dlm", "o", "STANDARD")
	put := func(key string) {
		o := &meta.Object{
			BucketID:     b.ID,
			Key:          key,
			StorageClass: "STANDARD",
			ETag:         "x",
			Size:         1,
			Mtime:        time.Now().UTC(),
			Manifest:     &data.Manifest{Class: "STANDARD"},
		}
		if err := s.PutObject(ctx, o, true); err != nil {
			t.Fatal(err)
		}
	}
	for _, k := range []string{"asdf", "boo/bar", "boo/baz/xyzzy", "cquux/thud", "cquux/bla"} {
		put(k)
	}
	keysOf := func(r *meta.ListResult) []string {
		out := make([]string, 0, len(r.Objects))
		for _, o := range r.Objects {
			out = append(out, o.Key)
		}
		return out
	}
	eq := func(a, b []string) bool {
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
	mustList := func(prefix, marker string, limit int) *meta.ListResult {
		r, err := s.ListObjects(ctx, b.ID, meta.ListOptions{
			Prefix: prefix, Delimiter: "/", Marker: marker, Limit: limit,
		})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}

	r := mustList("", "", 1)
	if !r.Truncated || !eq(keysOf(r), []string{"asdf"}) || len(r.CommonPrefixes) != 0 || r.NextMarker != "asdf" {
		t.Fatalf("p1: %+v keys=%v", r, keysOf(r))
	}
	r = mustList("", r.NextMarker, 1)
	if !r.Truncated || len(r.Objects) != 0 || !eq(r.CommonPrefixes, []string{"boo/"}) || r.NextMarker != "boo/" {
		t.Fatalf("p2: %+v", r)
	}
	r = mustList("", r.NextMarker, 1)
	if r.Truncated || len(r.Objects) != 0 || !eq(r.CommonPrefixes, []string{"cquux/"}) || r.NextMarker != "" {
		t.Fatalf("p3: %+v", r)
	}

	r = mustList("boo/", "", 1)
	if !r.Truncated || !eq(keysOf(r), []string{"boo/bar"}) || len(r.CommonPrefixes) != 0 || r.NextMarker != "boo/bar" {
		t.Fatalf("boo p1: %+v", r)
	}
	r = mustList("boo/", r.NextMarker, 1)
	if r.Truncated || len(r.Objects) != 0 || !eq(r.CommonPrefixes, []string{"boo/baz/"}) || r.NextMarker != "" {
		t.Fatalf("boo p2: %+v", r)
	}
}
