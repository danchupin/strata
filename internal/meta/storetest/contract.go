// Package storetest ships a shared contract test that any meta.Store
// implementation must pass. The memory store runs this suite unconditionally;
// Cassandra runs it under -tags integration.
package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/gocql/gocql"
	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/meta"
)

func newTimeUUID() string { return gocql.TimeUUID().String() }

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
		{"MultipartCompletionRoundTrip", caseMultipartCompletion},
		{"BucketMfaDeleteRoundTrip", caseBucketMfaDelete},
		{"SSEWrapRotationRoundTrip", caseSSEWrapRotation},
		{"NotificationQueueRoundTrip", caseNotificationQueue},
		{"NotificationDLQRoundTrip", caseNotificationDLQ},
		{"ReplicationQueueRoundTrip", caseReplicationQueue},
		{"AccessLogBufferRoundTrip", caseAccessLogBuffer},
		{"AuditLogRoundTrip", caseAuditLog},
		{"AuditLogFiltered", caseAuditLogFiltered},
		{"AuditLogPartitionExport", caseAuditLogPartitionExport},
		{"VersioningNullSentinel", caseVersioningNullSentinel},
		{"VersioningNullListVersions", caseVersioningNullListVersions},
		{"VersioningSuspendedReplaceNull", caseVersioningSuspendedReplaceNull},
		{"AccessPointCRUD", caseAccessPointCRUD},
		{"OnlineReshard", caseOnlineReshard},
		{"ManifestRawRoundTrip", caseManifestRawRoundTrip},
		{"AdminJobRoundTrip", caseAdminJobRoundTrip},
		{"ManagedPolicyCRUD", caseManagedPolicyCRUD},
		{"UserPolicyAttach", caseUserPolicyAttach},
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
		{Cluster: "default", Pool: "p1", Namespace: "n1", OID: "o1", Size: 100},
		{Cluster: "cold-eu", Pool: "p2", OID: "o2", Size: 200},
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
	byOID := map[string]data.ChunkRef{}
	for _, e := range entries {
		byOID[e.Chunk.OID] = e.Chunk
	}
	if got := byOID["o1"]; got.Cluster != "default" || got.Pool != "p1" || got.Namespace != "n1" {
		t.Errorf("o1 round-trip lost fields: %+v", got)
	}
	if got := byOID["o2"]; got.Cluster != "cold-eu" || got.Pool != "p2" {
		t.Errorf("o2 round-trip lost cluster id: %+v", got)
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

func caseMultipartCompletion(t *testing.T, s meta.Store) {
	ctx := context.Background()
	b, _ := s.CreateBucket(ctx, "mpc", "o", "STANDARD")
	uploadID := newTimeUUID()
	rec := &meta.MultipartCompletion{
		BucketID:    b.ID,
		UploadID:    uploadID,
		Key:         "obj",
		ETag:        "deadbeef-1",
		VersionID:   "v1",
		Body:        []byte(`<?xml version="1.0"?><CompleteMultipartUploadResult><ETag>"deadbeef-1"</ETag></CompleteMultipartUploadResult>`),
		Headers:     map[string]string{"ETag": `"deadbeef-1"`, "x-amz-server-side-encryption": "AES256"},
		CompletedAt: time.Now().UTC(),
	}
	if err := s.RecordMultipartCompletion(ctx, rec, time.Hour); err != nil {
		t.Fatalf("record: %v", err)
	}

	got, err := s.GetMultipartCompletion(ctx, b.ID, uploadID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ETag != rec.ETag || got.Key != rec.Key || got.VersionID != rec.VersionID {
		t.Errorf("scalar mismatch: %+v", got)
	}
	if string(got.Body) != string(rec.Body) {
		t.Errorf("body mismatch: %q vs %q", got.Body, rec.Body)
	}
	if got.Headers["ETag"] != rec.Headers["ETag"] || got.Headers["x-amz-server-side-encryption"] != "AES256" {
		t.Errorf("headers mismatch: %+v", got.Headers)
	}

	if _, err := s.GetMultipartCompletion(ctx, b.ID, newTimeUUID()); err != meta.ErrMultipartCompletionNotFound {
		t.Errorf("missing record: got %v want ErrMultipartCompletionNotFound", err)
	}
}

func caseBucketMfaDelete(t *testing.T, s meta.Store) {
	ctx := context.Background()
	if _, err := s.CreateBucket(ctx, "mfd", "o", "STANDARD"); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, _ := s.GetBucket(ctx, "mfd")
	if got.MfaDelete != "" {
		t.Errorf("default MfaDelete should be empty, got %q", got.MfaDelete)
	}
	if err := s.SetBucketMfaDelete(ctx, "mfd", meta.MfaDeleteEnabled); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, _ = s.GetBucket(ctx, "mfd")
	if got.MfaDelete != meta.MfaDeleteEnabled {
		t.Errorf("MfaDelete after set: %q", got.MfaDelete)
	}
	if err := s.SetBucketMfaDelete(ctx, "mfd", meta.MfaDeleteDisabled); err != nil {
		t.Fatalf("set disabled: %v", err)
	}
	got, _ = s.GetBucket(ctx, "mfd")
	if got.MfaDelete != meta.MfaDeleteDisabled {
		t.Errorf("MfaDelete after disable: %q", got.MfaDelete)
	}
	if err := s.SetBucketMfaDelete(ctx, "missing", meta.MfaDeleteEnabled); err != meta.ErrBucketNotFound {
		t.Errorf("missing bucket: got %v want ErrBucketNotFound", err)
	}
}

func caseSSEWrapRotation(t *testing.T, s meta.Store) {
	ctx := context.Background()
	b, err := s.CreateBucket(ctx, "rot", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}

	o := &meta.Object{
		BucketID:     b.ID,
		Key:          "k",
		StorageClass: "STANDARD",
		ETag:         "deadbeef",
		Size:         5,
		Mtime:        time.Now().UTC(),
		Manifest:     &data.Manifest{Class: "STANDARD"},
		SSE:          "AES256",
		SSEKey:       []byte("wrapped-under-A"),
		SSEKeyID:     "A",
	}
	if err := s.PutObject(ctx, o, false); err != nil {
		t.Fatalf("put: %v", err)
	}

	if err := s.UpdateObjectSSEWrap(ctx, b.ID, "k", "", []byte("wrapped-under-B"), "B"); err != nil {
		t.Fatalf("update wrap: %v", err)
	}
	got, err := s.GetObject(ctx, b.ID, "k", "")
	if err != nil {
		t.Fatalf("get after rewrap: %v", err)
	}
	if got.SSEKeyID != "B" || string(got.SSEKey) != "wrapped-under-B" {
		t.Fatalf("post-rewrap row: SSEKeyID=%q SSEKey=%q", got.SSEKeyID, string(got.SSEKey))
	}

	if _, err := s.GetRewrapProgress(ctx, b.ID); err != meta.ErrNoRewrapProgress {
		t.Fatalf("progress before set: %v", err)
	}
	if err := s.SetRewrapProgress(ctx, &meta.RewrapProgress{
		BucketID: b.ID,
		TargetID: "B",
		LastKey:  "k",
		Complete: true,
	}); err != nil {
		t.Fatalf("set progress: %v", err)
	}
	prog, err := s.GetRewrapProgress(ctx, b.ID)
	if err != nil || !prog.Complete || prog.TargetID != "B" || prog.LastKey != "k" {
		t.Fatalf("progress: %+v err=%v", prog, err)
	}
}

func caseManifestRawRoundTrip(t *testing.T, s meta.Store) {
	ctx := context.Background()
	b, err := s.CreateBucket(ctx, "mraw", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}

	// PutObject under the active encoder format. Tests in the meta backends
	// don't pin SetManifestFormat — both backends MUST round-trip whichever
	// format the package default hands them.
	mf := &data.Manifest{
		Class:     "STANDARD",
		Size:      9,
		ChunkSize: 4 * 1024 * 1024,
		ETag:      `"abc"`,
	}
	o := &meta.Object{
		BucketID:     b.ID,
		Key:          "k",
		StorageClass: "STANDARD",
		ETag:         `"abc"`,
		Size:         9,
		Mtime:        time.Now().UTC(),
		Manifest:     mf,
	}
	if err := s.PutObject(ctx, o, false); err != nil {
		t.Fatalf("put: %v", err)
	}

	raw, err := s.GetObjectManifestRaw(ctx, b.ID, "k", "")
	if err != nil {
		t.Fatalf("get raw: %v", err)
	}
	if len(raw) == 0 {
		t.Fatalf("expected non-empty manifest blob")
	}
	if _, err := data.DecodeManifest(raw); err != nil {
		t.Fatalf("decode raw: %v", err)
	}

	// Force JSON, write back, verify GetObject still decodes correctly and
	// the raw blob now reads as JSON.
	jsonBlob, err := data.EncodeManifestJSON(mf)
	if err != nil {
		t.Fatalf("json encode: %v", err)
	}
	if err := s.UpdateObjectManifestRaw(ctx, b.ID, "k", "", jsonBlob); err != nil {
		t.Fatalf("update raw json: %v", err)
	}
	roundtripped, err := s.GetObject(ctx, b.ID, "k", "")
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if roundtripped.Manifest == nil || roundtripped.Manifest.Class != "STANDARD" || roundtripped.Manifest.Size != 9 {
		t.Fatalf("post-update manifest: %+v", roundtripped.Manifest)
	}
	rawJSON, err := s.GetObjectManifestRaw(ctx, b.ID, "k", "")
	if err != nil {
		t.Fatalf("get raw json: %v", err)
	}
	if !data.IsManifestJSON(rawJSON) {
		t.Fatalf("expected raw to be JSON after update; first byte=%q", rawJSON[:1])
	}

	// Flip to proto and verify the rewriter-style round-trip.
	protoBlob, err := data.EncodeManifestProto(mf)
	if err != nil {
		t.Fatalf("proto encode: %v", err)
	}
	if err := s.UpdateObjectManifestRaw(ctx, b.ID, "k", "", protoBlob); err != nil {
		t.Fatalf("update raw proto: %v", err)
	}
	rawProto, err := s.GetObjectManifestRaw(ctx, b.ID, "k", "")
	if err != nil {
		t.Fatalf("get raw proto: %v", err)
	}
	if data.IsManifestJSON(rawProto) {
		t.Fatalf("expected raw to be proto after update; first byte=%q", rawProto[:1])
	}

	// Missing object surfaces ErrObjectNotFound (not ErrBucketNotFound).
	if _, err := s.GetObjectManifestRaw(ctx, b.ID, "nope", ""); err != meta.ErrObjectNotFound {
		t.Fatalf("missing object: got %v want ErrObjectNotFound", err)
	}
	if err := s.UpdateObjectManifestRaw(ctx, b.ID, "nope", "", protoBlob); err != meta.ErrObjectNotFound {
		t.Fatalf("update missing: got %v want ErrObjectNotFound", err)
	}
}

func caseNotificationQueue(t *testing.T, s meta.Store) {
	ctx := context.Background()
	b, err := s.CreateBucket(ctx, "nfy", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	now := time.Now().UTC()
	evt := &meta.NotificationEvent{
		BucketID:   b.ID,
		Bucket:     b.Name,
		Key:        "img/cat.jpg",
		EventID:    newTimeUUID(),
		EventName:  "s3:ObjectCreated:Put",
		EventTime:  now,
		ConfigID:   "OnPut",
		TargetType: "topic",
		TargetARN:  "arn:aws:sns:us-east-1:0:t",
		Payload:    []byte(`{"Records":[{"eventName":"s3:ObjectCreated:Put"}]}`),
	}
	if err := s.EnqueueNotification(ctx, evt); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	got, err := s.ListPendingNotifications(ctx, b.ID, 100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d events want 1", len(got))
	}
	if got[0].EventName != evt.EventName || got[0].Key != evt.Key || got[0].ConfigID != evt.ConfigID {
		t.Fatalf("row: %+v", got[0])
	}
	if string(got[0].Payload) != string(evt.Payload) {
		t.Fatalf("payload: %q", string(got[0].Payload))
	}
	if err := s.AckNotification(ctx, got[0]); err != nil {
		t.Fatalf("ack: %v", err)
	}
	remaining, _ := s.ListPendingNotifications(ctx, b.ID, 100)
	if len(remaining) != 0 {
		t.Fatalf("after ack: %d remaining", len(remaining))
	}
}

func caseNotificationDLQ(t *testing.T, s meta.Store) {
	ctx := context.Background()
	b, err := s.CreateBucket(ctx, "dlq", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	now := time.Now().UTC()
	entry := &meta.NotificationDLQEntry{
		NotificationEvent: meta.NotificationEvent{
			BucketID:   b.ID,
			Bucket:     b.Name,
			Key:        "img/dog.jpg",
			EventID:    newTimeUUID(),
			EventName:  "s3:ObjectCreated:Put",
			EventTime:  now,
			ConfigID:   "OnPut",
			TargetType: "topic",
			TargetARN:  "arn:aws:sns:us-east-1:0:t",
			Payload:    []byte(`{"Records":[{"eventName":"s3:ObjectCreated:Put"}]}`),
		},
		Attempts:   6,
		Reason:     "endpoint returned 503",
		EnqueuedAt: now,
	}
	if err := s.EnqueueNotificationDLQ(ctx, entry); err != nil {
		t.Fatalf("enqueue dlq: %v", err)
	}
	got, err := s.ListNotificationDLQ(ctx, b.ID, 100)
	if err != nil {
		t.Fatalf("list dlq: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d dlq entries want 1", len(got))
	}
	if got[0].Attempts != 6 || got[0].Reason != entry.Reason || got[0].Key != entry.Key {
		t.Fatalf("dlq row: %+v", got[0])
	}
	if string(got[0].Payload) != string(entry.Payload) {
		t.Fatalf("payload: %q", string(got[0].Payload))
	}
}

func caseAccessLogBuffer(t *testing.T, s meta.Store) {
	ctx := context.Background()
	b, err := s.CreateBucket(ctx, "alog", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	now := time.Now().UTC()
	entry := &meta.AccessLogEntry{
		BucketID:    b.ID,
		Bucket:      b.Name,
		EventID:     newTimeUUID(),
		Time:        now,
		RequestID:   "req-123",
		Principal:   "alice",
		SourceIP:    "10.0.0.1",
		Op:          "REST.PUT.OBJECT",
		Key:         "img/cat.jpg",
		Status:      200,
		BytesSent:   1024,
		ObjectSize:  4096,
		TotalTimeMS: 12,
		Referrer:    "https://example.com/",
		UserAgent:   "aws-cli/2.0",
		VersionID:   "",
	}
	if err := s.EnqueueAccessLog(ctx, entry); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	got, err := s.ListPendingAccessLog(ctx, b.ID, 100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows want 1", len(got))
	}
	if got[0].Op != entry.Op || got[0].Key != entry.Key || got[0].Status != entry.Status ||
		got[0].BytesSent != entry.BytesSent || got[0].ObjectSize != entry.ObjectSize ||
		got[0].RequestID != entry.RequestID || got[0].Principal != entry.Principal ||
		got[0].SourceIP != entry.SourceIP || got[0].Referrer != entry.Referrer ||
		got[0].UserAgent != entry.UserAgent {
		t.Fatalf("row: %+v", got[0])
	}
	if err := s.AckAccessLog(ctx, got[0]); err != nil {
		t.Fatalf("ack: %v", err)
	}
	remaining, _ := s.ListPendingAccessLog(ctx, b.ID, 100)
	if len(remaining) != 0 {
		t.Fatalf("after ack: %d remaining", len(remaining))
	}
}

func caseAuditLog(t *testing.T, s meta.Store) {
	ctx := context.Background()
	b, err := s.CreateBucket(ctx, "audit-bkt", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	now := time.Now().UTC()
	row := &meta.AuditEvent{
		BucketID:  b.ID,
		Bucket:    b.Name,
		EventID:   newTimeUUID(),
		Time:      now,
		Principal: "alice",
		Action:    "PutObject",
		Resource:  "/audit-bkt/img.jpg",
		Result:    "200",
		RequestID: "req-xyz",
		SourceIP:  "10.0.0.5",
	}
	if err := s.EnqueueAudit(ctx, row, time.Hour); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	got, err := s.ListAudit(ctx, b.ID, 100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows want 1", len(got))
	}
	g := got[0]
	if g.Principal != row.Principal || g.Action != row.Action || g.Resource != row.Resource ||
		g.Result != row.Result || g.RequestID != row.RequestID || g.SourceIP != row.SourceIP ||
		g.Bucket != row.Bucket {
		t.Fatalf("row: %+v", g)
	}
}

func caseAuditLogFiltered(t *testing.T, s meta.Store) {
	ctx := context.Background()
	b1, err := s.CreateBucket(ctx, "filt-a", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("create bucket a: %v", err)
	}
	b2, err := s.CreateBucket(ctx, "filt-b", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("create bucket b: %v", err)
	}
	now := time.Now().UTC()
	mk := func(bID uuid.UUID, bName, principal, action string, t time.Time) *meta.AuditEvent {
		return &meta.AuditEvent{
			BucketID:  bID,
			Bucket:    bName,
			EventID:   newTimeUUID(),
			Time:      t,
			Principal: principal,
			Action:    action,
			Resource:  "/" + bName,
			Result:    "200",
			RequestID: "req-" + action,
			SourceIP:  "10.0.0.1",
		}
	}
	rows := []*meta.AuditEvent{
		mk(b1.ID, b1.Name, "alice", "PutObject", now.Add(-3*time.Hour)),
		mk(b1.ID, b1.Name, "bob", "DeleteObject", now.Add(-2*time.Hour)),
		mk(b2.ID, b2.Name, "alice", "PutBucketCors", now.Add(-1*time.Hour)),
		mk(b2.ID, b2.Name, "bob", "PutObject", now.Add(-30*time.Minute)),
	}
	for _, r := range rows {
		if err := s.EnqueueAudit(ctx, r, time.Hour); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}

	// Bucket scope.
	got, _, err := s.ListAuditFiltered(ctx, meta.AuditFilter{BucketID: b1.ID, BucketScoped: true, Limit: 10})
	if err != nil {
		t.Fatalf("filter bucket: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("bucket-scoped len=%d want 2", len(got))
	}
	for _, e := range got {
		if e.Bucket != b1.Name {
			t.Fatalf("bucket leak: %+v", e)
		}
	}

	// Principal filter, no bucket scope.
	got, _, err = s.ListAuditFiltered(ctx, meta.AuditFilter{Principal: "alice", Limit: 10})
	if err != nil {
		t.Fatalf("filter principal: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("principal len=%d want 2", len(got))
	}
	for _, e := range got {
		if e.Principal != "alice" {
			t.Fatalf("principal leak: %+v", e)
		}
	}

	// Time window.
	got, _, err = s.ListAuditFiltered(ctx, meta.AuditFilter{
		Start: now.Add(-90 * time.Minute),
		End:   now,
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("filter time: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("time len=%d want 2", len(got))
	}

	// Combined filters: bucket b2 + principal bob.
	got, _, err = s.ListAuditFiltered(ctx, meta.AuditFilter{
		BucketID:     b2.ID,
		BucketScoped: true,
		Principal:    "bob",
		Limit:        10,
	})
	if err != nil {
		t.Fatalf("combined: %v", err)
	}
	if len(got) != 1 || got[0].Action != "PutObject" || got[0].Bucket != b2.Name {
		t.Fatalf("combined got=%+v", got)
	}

	// Pagination round-trip: limit=2 across 4 rows, no filters, walk pages.
	page1, next, err := s.ListAuditFiltered(ctx, meta.AuditFilter{Limit: 2})
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if len(page1) != 2 || next == "" {
		t.Fatalf("page1 size=%d next=%q", len(page1), next)
	}
	page2, next2, err := s.ListAuditFiltered(ctx, meta.AuditFilter{Limit: 2, Continuation: next})
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	if len(page2) != 2 {
		t.Fatalf("page2 size=%d", len(page2))
	}
	if next2 != "" {
		page3, _, err := s.ListAuditFiltered(ctx, meta.AuditFilter{Limit: 2, Continuation: next2})
		if err != nil {
			t.Fatalf("page3: %v", err)
		}
		if len(page3) != 0 {
			t.Fatalf("page3 size=%d want 0", len(page3))
		}
	}
	seen := map[string]bool{}
	for _, e := range append(page1, page2...) {
		if seen[e.EventID] {
			t.Fatalf("duplicate eventID across pages: %s", e.EventID)
		}
		seen[e.EventID] = true
	}
	if len(seen) != 4 {
		t.Fatalf("paginated rows %d want 4", len(seen))
	}
}

// caseAuditLogPartitionExport exercises the US-046 strata-audit-export
// surface: ListAuditPartitionsBefore enumerates fully-aged (bucket, day)
// partitions, ReadAuditPartition returns every row in deterministic order,
// and DeleteAuditPartition drops the partition without disturbing
// younger-day rows in the same bucket.
func caseAuditLogPartitionExport(t *testing.T, s meta.Store) {
	ctx := context.Background()
	b1, err := s.CreateBucket(ctx, "exp-a", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("create bucket a: %v", err)
	}
	b2, err := s.CreateBucket(ctx, "exp-b", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("create bucket b: %v", err)
	}
	now := time.Now().UTC()
	mk := func(bID uuid.UUID, bName string, when time.Time) *meta.AuditEvent {
		return &meta.AuditEvent{
			BucketID:  bID,
			Bucket:    bName,
			EventID:   newTimeUUID(),
			Time:      when,
			Principal: "alice",
			Action:    "PutObject",
			Resource:  "/" + bName + "/k",
			Result:    "200",
			RequestID: "req",
			SourceIP:  "10.0.0.1",
		}
	}
	// Two old days for b1 (40d, 35d), one old day for b2 (32d), one fresh
	// row for b1 (1d) that must NOT show up as an exportable partition.
	old1 := now.AddDate(0, 0, -40)
	old2 := now.AddDate(0, 0, -35)
	old3 := now.AddDate(0, 0, -32)
	fresh := now.AddDate(0, 0, -1)
	for _, evt := range []*meta.AuditEvent{
		mk(b1.ID, b1.Name, old1),
		mk(b1.ID, b1.Name, old1.Add(time.Hour)),
		mk(b1.ID, b1.Name, old2),
		mk(b2.ID, b2.Name, old3),
		mk(b1.ID, b1.Name, fresh),
	} {
		if err := s.EnqueueAudit(ctx, evt, time.Hour); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}

	cutoff := now.AddDate(0, 0, -30)
	parts, err := s.ListAuditPartitionsBefore(ctx, cutoff)
	if err != nil {
		t.Fatalf("list partitions: %v", err)
	}
	if len(parts) != 3 {
		t.Fatalf("partitions=%d want 3 (%+v)", len(parts), parts)
	}
	totalRows := 0
	for _, p := range parts {
		rows, err := s.ReadAuditPartition(ctx, p.BucketID, p.Day)
		if err != nil {
			t.Fatalf("read partition: %v", err)
		}
		for i := 1; i < len(rows); i++ {
			if rows[i-1].EventID > rows[i].EventID {
				t.Fatalf("partition %s/%s rows not sorted by event_id", p.Bucket, p.Day)
			}
		}
		totalRows += len(rows)
		if err := s.DeleteAuditPartition(ctx, p.BucketID, p.Day); err != nil {
			t.Fatalf("delete partition: %v", err)
		}
		left, err := s.ReadAuditPartition(ctx, p.BucketID, p.Day)
		if err != nil {
			t.Fatalf("re-read partition: %v", err)
		}
		if len(left) != 0 {
			t.Fatalf("partition not empty after delete: %d rows", len(left))
		}
	}
	if totalRows != 4 {
		t.Fatalf("exported rows=%d want 4", totalRows)
	}

	// Fresh partition for b1 still has its row.
	rest, err := s.ListAudit(ctx, b1.ID, 100)
	if err != nil {
		t.Fatalf("list remaining: %v", err)
	}
	if len(rest) != 1 {
		t.Fatalf("fresh row missing or stale rows kept: %+v", rest)
	}

	// After deleting all aged partitions, ListAuditPartitionsBefore is empty.
	parts2, err := s.ListAuditPartitionsBefore(ctx, cutoff)
	if err != nil {
		t.Fatalf("re-list partitions: %v", err)
	}
	if len(parts2) != 0 {
		t.Fatalf("expected zero aged partitions after delete, got %d", len(parts2))
	}
}

func caseReplicationQueue(t *testing.T, s meta.Store) {
	ctx := context.Background()
	b, err := s.CreateBucket(ctx, "rep", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	now := time.Now().UTC()
	evt := &meta.ReplicationEvent{
		BucketID:          b.ID,
		Bucket:            b.Name,
		Key:               "logs/2026/04/x.txt",
		VersionID:         newTimeUUID(),
		EventID:           newTimeUUID(),
		EventName:         "s3:Replication:Pending",
		EventTime:         now,
		RuleID:            "logs",
		DestinationBucket: "arn:aws:s3:::dest",
		StorageClass:      "STANDARD",
	}
	if err := s.EnqueueReplication(ctx, evt); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	got, err := s.ListPendingReplications(ctx, b.ID, 100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d events want 1", len(got))
	}
	if got[0].RuleID != evt.RuleID || got[0].Key != evt.Key || got[0].DestinationBucket != evt.DestinationBucket {
		t.Fatalf("row: %+v", got[0])
	}
	if got[0].VersionID != evt.VersionID {
		t.Fatalf("version: got %q want %q", got[0].VersionID, evt.VersionID)
	}
	if err := s.AckReplication(ctx, got[0]); err != nil {
		t.Fatalf("ack: %v", err)
	}
	remaining, _ := s.ListPendingReplications(ctx, b.ID, 100)
	if len(remaining) != 0 {
		t.Fatalf("after ack: %d remaining", len(remaining))
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

func caseVersioningNullSentinel(t *testing.T, s meta.Store) {
	ctx := context.Background()
	b, err := s.CreateBucket(ctx, "nullv", "o", "STANDARD")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if b.Versioning != meta.VersioningDisabled {
		t.Fatalf("default versioning: %s", b.Versioning)
	}
	o := &meta.Object{
		BucketID:     b.ID,
		Key:          "doc",
		StorageClass: "STANDARD",
		ETag:         "first",
		Size:         5,
		Mtime:        time.Now().UTC(),
		Manifest:     &data.Manifest{Class: "STANDARD", Size: 5},
	}
	if err := s.PutObject(ctx, o, false); err != nil {
		t.Fatalf("put: %v", err)
	}
	if o.VersionID != meta.NullVersionID {
		t.Fatalf("VersionID after Disabled PUT: got %q want %q", o.VersionID, meta.NullVersionID)
	}
	if !o.IsNull {
		t.Fatalf("IsNull after Disabled PUT: got false want true")
	}

	bySentinel, err := s.GetObject(ctx, b.ID, "doc", meta.NullVersionID)
	if err != nil {
		t.Fatalf("get by sentinel: %v", err)
	}
	if bySentinel.ETag != "first" || !bySentinel.IsNull || bySentinel.VersionID != meta.NullVersionID {
		t.Fatalf("by-sentinel row: %+v", bySentinel)
	}

	byLiteral, err := s.GetObject(ctx, b.ID, "doc", meta.NullVersionLiteral)
	if err != nil {
		t.Fatalf("get by 'null' literal: %v", err)
	}
	if byLiteral.ETag != "first" || !byLiteral.IsNull || byLiteral.VersionID != meta.NullVersionID {
		t.Fatalf("by-literal row: %+v", byLiteral)
	}

	latest, err := s.GetObject(ctx, b.ID, "doc", "")
	if err != nil {
		t.Fatalf("get latest: %v", err)
	}
	if latest.ETag != "first" || !latest.IsNull {
		t.Fatalf("latest: %+v", latest)
	}

	// Overwrite under Disabled mode: same sentinel, new content.
	o2 := &meta.Object{
		BucketID:     b.ID,
		Key:          "doc",
		StorageClass: "STANDARD",
		ETag:         "second",
		Size:         6,
		Mtime:        time.Now().UTC(),
		Manifest:     &data.Manifest{Class: "STANDARD", Size: 6},
	}
	if err := s.PutObject(ctx, o2, false); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	if o2.VersionID != meta.NullVersionID {
		t.Fatalf("overwrite VersionID: %q", o2.VersionID)
	}
	got, err := s.GetObject(ctx, b.ID, "doc", meta.NullVersionLiteral)
	if err != nil {
		t.Fatalf("get after overwrite: %v", err)
	}
	if got.ETag != "second" {
		t.Fatalf("after overwrite ETag=%q want second", got.ETag)
	}
}

// caseVersioningNullListVersions covers US-028: the null-versioned ancestor
// remains addressable after toggling the bucket from Disabled to Enabled, an
// Enabled-mode PUT prepends a TimeUUID version above the null one without
// overwriting it, and ListObjectVersions surfaces both rows with the null one
// flagged IsNull=true.
func caseVersioningNullListVersions(t *testing.T, s meta.Store) {
	ctx := context.Background()
	b, err := s.CreateBucket(ctx, "nlv", "o", "STANDARD")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	put := func(body string, versioned bool) string {
		o := &meta.Object{
			BucketID:     b.ID,
			Key:          "doc",
			StorageClass: "STANDARD",
			ETag:         body,
			Size:         int64(len(body)),
			Mtime:        time.Now().UTC(),
			Manifest:     &data.Manifest{Class: "STANDARD", Size: int64(len(body))},
		}
		if err := s.PutObject(ctx, o, versioned); err != nil {
			t.Fatalf("put %q versioned=%v: %v", body, versioned, err)
		}
		return o.VersionID
	}

	// 1) Disabled-mode PUT lands the null-versioned row.
	nullV := put("first", false)
	if nullV != meta.NullVersionID {
		t.Fatalf("disabled put VersionID=%q want sentinel", nullV)
	}

	// 2) Toggle to Enabled; null row stays addressable as ?versionId=null.
	if err := s.SetBucketVersioning(ctx, "nlv", meta.VersioningEnabled); err != nil {
		t.Fatalf("set versioning: %v", err)
	}
	gotNull, err := s.GetObject(ctx, b.ID, "doc", meta.NullVersionLiteral)
	if err != nil {
		t.Fatalf("get null after toggle: %v", err)
	}
	if !gotNull.IsNull || gotNull.ETag != "first" {
		t.Fatalf("null after toggle: %+v", gotNull)
	}

	// 3) Enabled-mode PUT prepends a TimeUUID version. Null row preserved.
	v2 := put("second", true)
	if v2 == meta.NullVersionID || v2 == "" {
		t.Fatalf("enabled put VersionID=%q want fresh TimeUUID", v2)
	}

	latest, err := s.GetObject(ctx, b.ID, "doc", "")
	if err != nil {
		t.Fatalf("get latest: %v", err)
	}
	if latest.VersionID != v2 || latest.ETag != "second" || latest.IsNull {
		t.Fatalf("latest: %+v", latest)
	}

	stillNull, err := s.GetObject(ctx, b.ID, "doc", meta.NullVersionLiteral)
	if err != nil {
		t.Fatalf("get null after enabled put: %v", err)
	}
	if !stillNull.IsNull || stillNull.ETag != "first" {
		t.Fatalf("null after enabled put: %+v", stillNull)
	}

	// 4) ListObjectVersions surfaces both rows with correct IsLatest + IsNull.
	res, err := s.ListObjectVersions(ctx, b.ID, meta.ListOptions{Limit: 100})
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	if len(res.Versions) != 2 {
		t.Fatalf("got %d versions want 2", len(res.Versions))
	}
	var sawLatest, sawNull bool
	for _, v := range res.Versions {
		switch v.VersionID {
		case v2:
			if !v.IsLatest || v.IsNull || v.ETag != "second" {
				t.Fatalf("latest entry: %+v", v)
			}
			sawLatest = true
		case meta.NullVersionID:
			if v.IsLatest {
				t.Fatalf("null entry should not be latest: %+v", v)
			}
			if !v.IsNull || v.ETag != "first" {
				t.Fatalf("null entry: %+v", v)
			}
			sawNull = true
		default:
			t.Fatalf("unexpected versionID %q in list", v.VersionID)
		}
	}
	if !sawLatest || !sawNull {
		t.Fatalf("missing entries (sawLatest=%v sawNull=%v)", sawLatest, sawNull)
	}
}

// caseVersioningSuspendedReplaceNull covers US-029: in Suspended-versioning
// mode, an unversioned PUT atomically replaces any prior null-versioned row
// (preserving TimeUUID-versioned ancestors), and an unversioned DELETE
// atomically writes a null-versioned delete marker that replaces the prior
// null row in the same way.
func caseVersioningSuspendedReplaceNull(t *testing.T, s meta.Store) {
	ctx := context.Background()
	b, err := s.CreateBucket(ctx, "susp", "o", "STANDARD")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	put := func(body string, versioned, suspended bool) string {
		o := &meta.Object{
			BucketID:     b.ID,
			Key:          "doc",
			StorageClass: "STANDARD",
			ETag:         body,
			Size:         int64(len(body)),
			Mtime:        time.Now().UTC(),
			Manifest:     &data.Manifest{Class: "STANDARD", Size: int64(len(body))},
		}
		if suspended {
			o.IsNull = true
		}
		if err := s.PutObject(ctx, o, versioned); err != nil {
			t.Fatalf("put %q: %v", body, err)
		}
		return o.VersionID
	}

	// 1) Enabled-mode PUT lands a TimeUUID version v1.
	if err := s.SetBucketVersioning(ctx, "susp", meta.VersioningEnabled); err != nil {
		t.Fatalf("set enabled: %v", err)
	}
	v1 := put("first", true, false)
	if v1 == meta.NullVersionID || v1 == "" {
		t.Fatalf("v1 VersionID=%q want fresh TimeUUID", v1)
	}

	// 2) Toggle to Suspended; unversioned PUT writes a null-versioned row
	// alongside v1.
	if err := s.SetBucketVersioning(ctx, "susp", meta.VersioningSuspended); err != nil {
		t.Fatalf("set suspended: %v", err)
	}
	nullV := put("second", true, true)
	if nullV != meta.NullVersionID {
		t.Fatalf("suspended put VersionID=%q want sentinel", nullV)
	}

	// 3) v1 is still addressable.
	gotV1, err := s.GetObject(ctx, b.ID, "doc", v1)
	if err != nil {
		t.Fatalf("get v1: %v", err)
	}
	if gotV1.ETag != "first" || gotV1.IsNull {
		t.Fatalf("v1 row: %+v", gotV1)
	}

	// 4) Latest is the null row.
	latest, err := s.GetObject(ctx, b.ID, "doc", "")
	if err != nil {
		t.Fatalf("get latest: %v", err)
	}
	if latest.ETag != "second" || !latest.IsNull || latest.VersionID != meta.NullVersionID {
		t.Fatalf("latest after suspended put: %+v", latest)
	}

	// 5) Suspended PUT again replaces the null row in place; v1 still present.
	put("third", true, true)
	gotNull, err := s.GetObject(ctx, b.ID, "doc", meta.NullVersionLiteral)
	if err != nil {
		t.Fatalf("get null after replace: %v", err)
	}
	if gotNull.ETag != "third" {
		t.Fatalf("null after replace ETag=%q want third", gotNull.ETag)
	}
	gotV1, err = s.GetObject(ctx, b.ID, "doc", v1)
	if err != nil {
		t.Fatalf("get v1 after suspended replace: %v", err)
	}
	if gotV1.ETag != "first" {
		t.Fatalf("v1 lost after suspended replace: %+v", gotV1)
	}

	// 6) Suspended unversioned DELETE writes a null-versioned delete marker
	// that replaces the prior null row; v1 untouched.
	dm, err := s.DeleteObjectNullReplacement(ctx, b.ID, "doc")
	if err != nil {
		t.Fatalf("delete null replacement: %v", err)
	}
	if !dm.IsDeleteMarker || !dm.IsNull || dm.VersionID != meta.NullVersionID {
		t.Fatalf("delete marker: %+v", dm)
	}
	if _, err := s.GetObject(ctx, b.ID, "doc", ""); err != meta.ErrObjectNotFound {
		t.Fatalf("get latest after marker: got err %v want ErrObjectNotFound", err)
	}
	gotV1, err = s.GetObject(ctx, b.ID, "doc", v1)
	if err != nil {
		t.Fatalf("get v1 after marker: %v", err)
	}
	if gotV1.ETag != "first" {
		t.Fatalf("v1 lost after marker: %+v", gotV1)
	}

	// 7) ListObjectVersions sees the null delete marker (latest) + v1.
	res, err := s.ListObjectVersions(ctx, b.ID, meta.ListOptions{Limit: 100})
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	if len(res.Versions) != 2 {
		t.Fatalf("got %d versions want 2: %+v", len(res.Versions), res.Versions)
	}
	var sawMarker, sawV1 bool
	for _, v := range res.Versions {
		switch v.VersionID {
		case meta.NullVersionID:
			if !v.IsLatest || !v.IsDeleteMarker || !v.IsNull {
				t.Fatalf("marker entry: %+v", v)
			}
			sawMarker = true
		case v1:
			if v.IsLatest || v.IsNull || v.ETag != "first" {
				t.Fatalf("v1 entry: %+v", v)
			}
			sawV1 = true
		default:
			t.Fatalf("unexpected versionID %q", v.VersionID)
		}
	}
	if !sawMarker || !sawV1 {
		t.Fatalf("missing entries (marker=%v v1=%v)", sawMarker, sawV1)
	}
}

func caseAccessPointCRUD(t *testing.T, s meta.Store) {
	ctx := context.Background()
	b, err := s.CreateBucket(ctx, "ap-bkt", "owner-a", "STANDARD")
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	ap := &meta.AccessPoint{
		Name:              "ap-one",
		BucketID:          b.ID,
		Bucket:            b.Name,
		Alias:             "ap-aaaaaaaaaaaa",
		NetworkOrigin:     "Internet",
		Policy:            []byte(`{"Version":"2012-10-17"}`),
		PublicAccessBlock: []byte(`<PublicAccessBlockConfiguration/>`),
		CreatedAt:         time.Now().UTC().Truncate(time.Millisecond),
	}
	if err := s.CreateAccessPoint(ctx, ap); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.CreateAccessPoint(ctx, ap); err != meta.ErrAccessPointAlreadyExists {
		t.Fatalf("dup create: got %v want ErrAccessPointAlreadyExists", err)
	}
	got, err := s.GetAccessPoint(ctx, "ap-one")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != ap.Name || got.Bucket != ap.Bucket || got.Alias != ap.Alias ||
		got.NetworkOrigin != ap.NetworkOrigin || got.BucketID != ap.BucketID {
		t.Fatalf("get round-trip: %+v", got)
	}
	if string(got.Policy) != string(ap.Policy) {
		t.Fatalf("policy round-trip: %q", got.Policy)
	}
	if string(got.PublicAccessBlock) != string(ap.PublicAccessBlock) {
		t.Fatalf("pab round-trip: %q", got.PublicAccessBlock)
	}

	if _, err := s.GetAccessPoint(ctx, "missing"); err != meta.ErrAccessPointNotFound {
		t.Fatalf("get missing: got %v want ErrAccessPointNotFound", err)
	}

	byAlias, err := s.GetAccessPointByAlias(ctx, ap.Alias)
	if err != nil {
		t.Fatalf("get by alias: %v", err)
	}
	if byAlias.Name != ap.Name || byAlias.Bucket != ap.Bucket {
		t.Fatalf("by alias round-trip: %+v", byAlias)
	}
	if _, err := s.GetAccessPointByAlias(ctx, "ap-missing"); err != meta.ErrAccessPointNotFound {
		t.Fatalf("by alias missing: got %v want ErrAccessPointNotFound", err)
	}

	list, err := s.ListAccessPoints(ctx, uuid.Nil)
	if err != nil || len(list) != 1 || list[0].Name != "ap-one" {
		t.Fatalf("list all: err=%v list=%+v", err, list)
	}
	listForBucket, err := s.ListAccessPoints(ctx, b.ID)
	if err != nil || len(listForBucket) != 1 {
		t.Fatalf("list scoped: err=%v list=%+v", err, listForBucket)
	}
	listOther, err := s.ListAccessPoints(ctx, uuid.New())
	if err != nil || len(listOther) != 0 {
		t.Fatalf("list other: err=%v list=%+v", err, listOther)
	}

	if err := s.DeleteAccessPoint(ctx, "ap-one"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := s.DeleteAccessPoint(ctx, "ap-one"); err != meta.ErrAccessPointNotFound {
		t.Fatalf("delete missing: got %v want ErrAccessPointNotFound", err)
	}
	list, err = s.ListAccessPoints(ctx, uuid.Nil)
	if err != nil || len(list) != 0 {
		t.Fatalf("list after delete: err=%v list=%+v", err, list)
	}
}

// caseOnlineReshard exercises the US-045 reshard state machine end-to-end.
// 1000 objects, 32→128 (memory backend may default to ShardCount=64; the test
// just validates the contract is honoured: target stamped, list invariant,
// CompleteReshard rotates active and clears target).
func caseOnlineReshard(t *testing.T, s meta.Store) {
	ctx := context.Background()
	b, err := s.CreateBucket(ctx, "rsh", "o", "STANDARD")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1000; i++ {
		key := "k" + padInt(i, 4)
		o := &meta.Object{
			BucketID: b.ID, Key: key,
			StorageClass: "STANDARD", ETag: "e", Size: 1,
			Mtime:    time.Now().UTC(),
			Manifest: &data.Manifest{Class: "STANDARD"},
		}
		if err := s.PutObject(ctx, o, false); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}

	bucket, _ := s.GetBucket(ctx, "rsh")
	startTarget := bucket.ShardCount * 4
	if !meta.IsValidShardCount(startTarget) {
		t.Fatalf("test backend ShardCount must be a power of two, got %d", bucket.ShardCount)
	}

	job, err := s.StartReshard(ctx, b.ID, startTarget)
	if err != nil {
		t.Fatalf("start reshard: %v", err)
	}
	if job.Source != bucket.ShardCount || job.Target != startTarget {
		t.Fatalf("job fields: %+v", job)
	}
	if _, err := s.StartReshard(ctx, b.ID, startTarget*2); err != meta.ErrReshardInProgress {
		t.Fatalf("second start: got %v want ErrReshardInProgress", err)
	}

	bucket, _ = s.GetBucket(ctx, "rsh")
	if bucket.TargetShardCount != startTarget {
		t.Fatalf("target after start: got %d want %d", bucket.TargetShardCount, startTarget)
	}

	res, err := s.ListObjects(ctx, b.ID, meta.ListOptions{Limit: 5000})
	if err != nil {
		t.Fatalf("list during reshard: %v", err)
	}
	if len(res.Objects) != 1000 {
		t.Fatalf("list during reshard: %d want 1000", len(res.Objects))
	}

	jobs, err := s.ListReshardJobs(ctx)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("list jobs during run: %v len=%d", err, len(jobs))
	}

	if err := s.CompleteReshard(ctx, b.ID); err != nil {
		t.Fatalf("complete: %v", err)
	}

	bucket, _ = s.GetBucket(ctx, "rsh")
	if bucket.ShardCount != startTarget {
		t.Fatalf("post-complete shard count: %d want %d", bucket.ShardCount, startTarget)
	}
	if bucket.TargetShardCount != 0 {
		t.Fatalf("post-complete target: %d want 0", bucket.TargetShardCount)
	}
	if _, err := s.GetReshardJob(ctx, b.ID); err != meta.ErrReshardNotFound {
		t.Fatalf("post-complete get: %v want ErrReshardNotFound", err)
	}

	res, err = s.ListObjects(ctx, b.ID, meta.ListOptions{Limit: 5000})
	if err != nil || len(res.Objects) != 1000 {
		t.Fatalf("list after reshard: err=%v len=%d", err, len(res.Objects))
	}

	if _, err := s.StartReshard(ctx, b.ID, 7); err != meta.ErrReshardInvalidTarget {
		t.Fatalf("non-power-of-two target: %v want ErrReshardInvalidTarget", err)
	}
}

// caseAdminJobRoundTrip exercises the AdminJob CRUD surface used by US-002
// (the embedded console's force-empty job). Covers create + duplicate +
// not-found + state forward roll.
func caseAdminJobRoundTrip(t *testing.T, s meta.Store) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	job := &meta.AdminJob{
		ID:        uuid.NewString(),
		Kind:      meta.AdminJobKindForceEmpty,
		Bucket:    "fe-bucket",
		State:     meta.AdminJobStatePending,
		StartedAt: now,
		UpdatedAt: now,
	}
	if err := s.CreateAdminJob(ctx, job); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.CreateAdminJob(ctx, job); err != meta.ErrAdminJobAlreadyExists {
		t.Errorf("duplicate: got %v want ErrAdminJobAlreadyExists", err)
	}
	got, err := s.GetAdminJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Kind != job.Kind || got.Bucket != job.Bucket || got.State != meta.AdminJobStatePending {
		t.Fatalf("get round-trip: %+v", got)
	}

	got.State = meta.AdminJobStateRunning
	got.Deleted = 42
	got.UpdatedAt = now.Add(time.Second)
	if err := s.UpdateAdminJob(ctx, got); err != nil {
		t.Fatalf("update: %v", err)
	}
	got2, err := s.GetAdminJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("re-get: %v", err)
	}
	if got2.State != meta.AdminJobStateRunning || got2.Deleted != 42 {
		t.Fatalf("update round-trip: %+v", got2)
	}

	if _, err := s.GetAdminJob(ctx, "nonexistent"); err != meta.ErrAdminJobNotFound {
		t.Errorf("missing get: %v want ErrAdminJobNotFound", err)
	}
	if err := s.UpdateAdminJob(ctx, &meta.AdminJob{ID: "nonexistent"}); err != meta.ErrAdminJobNotFound {
		t.Errorf("missing update: %v want ErrAdminJobNotFound", err)
	}
}

// caseManagedPolicyCRUD exercises the ManagedPolicy storage surface (US-010):
// create + duplicate + get + list (including path-prefix filter) + update +
// delete + sentinel errors on missing rows.
func caseManagedPolicyCRUD(t *testing.T, s meta.Store) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	first := &meta.ManagedPolicy{
		Arn:         "arn:aws:iam::strata:policy/AdminAccess",
		Name:        "AdminAccess",
		Path:        "/",
		Description: "operator full access",
		Document:    []byte(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"*","Resource":"*"}]}`),
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	second := &meta.ManagedPolicy{
		Arn:       "arn:aws:iam::strata:policy/team/ReadOnly",
		Name:      "ReadOnly",
		Path:      "/team/",
		Document:  []byte(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:Get*","Resource":"*"}]}`),
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.CreateManagedPolicy(ctx, first); err != nil {
		t.Fatalf("create first: %v", err)
	}
	if err := s.CreateManagedPolicy(ctx, first); err != meta.ErrManagedPolicyAlreadyExists {
		t.Errorf("duplicate: got %v want ErrManagedPolicyAlreadyExists", err)
	}
	if err := s.CreateManagedPolicy(ctx, second); err != nil {
		t.Fatalf("create second: %v", err)
	}

	got, err := s.GetManagedPolicy(ctx, first.Arn)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != first.Name || got.Path != first.Path || string(got.Document) != string(first.Document) {
		t.Fatalf("get round-trip: %+v", got)
	}

	all, err := s.ListManagedPolicies(ctx, "")
	if err != nil || len(all) != 2 {
		t.Fatalf("list all: err=%v len=%d", err, len(all))
	}
	if all[0].Arn != first.Arn || all[1].Arn != second.Arn {
		t.Fatalf("list ordering: %s,%s", all[0].Arn, all[1].Arn)
	}

	teamOnly, err := s.ListManagedPolicies(ctx, "/team/")
	if err != nil || len(teamOnly) != 1 || teamOnly[0].Arn != second.Arn {
		t.Fatalf("list path filter: err=%v %+v", err, teamOnly)
	}

	newDoc := []byte(`{"Version":"2012-10-17","Statement":[]}`)
	updatedAt := now.Add(time.Hour)
	if err := s.UpdateManagedPolicyDocument(ctx, first.Arn, newDoc, updatedAt); err != nil {
		t.Fatalf("update: %v", err)
	}
	got2, err := s.GetManagedPolicy(ctx, first.Arn)
	if err != nil {
		t.Fatalf("re-get: %v", err)
	}
	if string(got2.Document) != string(newDoc) {
		t.Fatalf("document not updated: %q", string(got2.Document))
	}
	if !got2.UpdatedAt.Equal(updatedAt) {
		t.Fatalf("updated_at: got %v want %v", got2.UpdatedAt, updatedAt)
	}

	if err := s.UpdateManagedPolicyDocument(ctx, "arn:aws:iam::strata:policy/Missing", newDoc, updatedAt); err != meta.ErrManagedPolicyNotFound {
		t.Errorf("update missing: got %v want ErrManagedPolicyNotFound", err)
	}
	if _, err := s.GetManagedPolicy(ctx, "arn:aws:iam::strata:policy/Missing"); err != meta.ErrManagedPolicyNotFound {
		t.Errorf("get missing: got %v want ErrManagedPolicyNotFound", err)
	}

	if err := s.DeleteManagedPolicy(ctx, first.Arn); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.GetManagedPolicy(ctx, first.Arn); err != meta.ErrManagedPolicyNotFound {
		t.Errorf("post-delete get: %v want ErrManagedPolicyNotFound", err)
	}
	if err := s.DeleteManagedPolicy(ctx, first.Arn); err != meta.ErrManagedPolicyNotFound {
		t.Errorf("delete missing: got %v want ErrManagedPolicyNotFound", err)
	}
}

// caseUserPolicyAttach exercises the user-policy attachment surface
// (US-010): attach + duplicate + list + detach + ErrPolicyAttached on
// DeleteManagedPolicy + missing-user / missing-policy sentinels.
func caseUserPolicyAttach(t *testing.T, s meta.Store) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	user := &meta.IAMUser{
		UserName:  "alice",
		UserID:    "AIDAALICEUUID",
		Path:      "/",
		CreatedAt: now,
	}
	if err := s.CreateIAMUser(ctx, user); err != nil {
		t.Fatalf("create user: %v", err)
	}
	policy := &meta.ManagedPolicy{
		Arn:       "arn:aws:iam::strata:policy/ReadOnly",
		Name:      "ReadOnly",
		Path:      "/",
		Document:  []byte(`{"Version":"2012-10-17","Statement":[]}`),
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.CreateManagedPolicy(ctx, policy); err != nil {
		t.Fatalf("create policy: %v", err)
	}

	if err := s.AttachUserPolicy(ctx, "ghost", policy.Arn); err != meta.ErrIAMUserNotFound {
		t.Errorf("attach missing user: got %v want ErrIAMUserNotFound", err)
	}
	if err := s.AttachUserPolicy(ctx, user.UserName, "arn:aws:iam::strata:policy/Ghost"); err != meta.ErrManagedPolicyNotFound {
		t.Errorf("attach missing policy: got %v want ErrManagedPolicyNotFound", err)
	}

	if err := s.AttachUserPolicy(ctx, user.UserName, policy.Arn); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if err := s.AttachUserPolicy(ctx, user.UserName, policy.Arn); err != meta.ErrUserPolicyAlreadyAttached {
		t.Errorf("dup attach: got %v want ErrUserPolicyAlreadyAttached", err)
	}

	attached, err := s.ListUserPolicies(ctx, user.UserName)
	if err != nil || len(attached) != 1 || attached[0] != policy.Arn {
		t.Fatalf("list: err=%v %+v", err, attached)
	}

	if _, err := s.ListUserPolicies(ctx, "ghost"); err != meta.ErrIAMUserNotFound {
		t.Errorf("list missing user: got %v want ErrIAMUserNotFound", err)
	}

	if err := s.DeleteManagedPolicy(ctx, policy.Arn); err != meta.ErrPolicyAttached {
		t.Errorf("delete attached policy: got %v want ErrPolicyAttached", err)
	}

	if err := s.DetachUserPolicy(ctx, user.UserName, "arn:aws:iam::strata:policy/Other"); err != meta.ErrUserPolicyNotAttached {
		t.Errorf("detach missing: got %v want ErrUserPolicyNotAttached", err)
	}
	if err := s.DetachUserPolicy(ctx, user.UserName, policy.Arn); err != nil {
		t.Fatalf("detach: %v", err)
	}
	if err := s.DetachUserPolicy(ctx, user.UserName, policy.Arn); err != meta.ErrUserPolicyNotAttached {
		t.Errorf("dup detach: got %v want ErrUserPolicyNotAttached", err)
	}

	post, err := s.ListUserPolicies(ctx, user.UserName)
	if err != nil || len(post) != 0 {
		t.Fatalf("post-detach list: err=%v %+v", err, post)
	}

	if err := s.DeleteManagedPolicy(ctx, policy.Arn); err != nil {
		t.Fatalf("delete after detach: %v", err)
	}
}

// padInt formats i with a fixed width using leading zeros.
func padInt(i, width int) string {
	s := []byte("0000000000")
	out := s[:width]
	pos := width - 1
	for i > 0 && pos >= 0 {
		out[pos] = byte('0' + i%10)
		i /= 10
		pos--
	}
	return string(out)
}

