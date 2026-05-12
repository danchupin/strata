package rebalance

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/meta"
	metamem "github.com/danchupin/strata/internal/meta/memory"
)

// fakeS3Cluster is an in-memory S3Cluster used by the unit tests. Keys
// are "bucket/key"; Get returns the raw bytes Put planted. Endpoint +
// Region drive the same-endpoint detect. Per-op counters surface so
// tests can assert traffic shape (server-side Copy vs Get/Put).
type fakeS3Cluster struct {
	id       string
	endpoint string
	region   string
	mu       sync.Mutex
	store    map[string][]byte
	gets     int
	puts     int
	copies   int
	getErr   error
	putErr   error
	copyErr  error
}

func newFakeS3Cluster(id, endpoint, region string) *fakeS3Cluster {
	return &fakeS3Cluster{id: id, endpoint: endpoint, region: region, store: map[string][]byte{}}
}

func (f *fakeS3Cluster) ID() string       { return f.id }
func (f *fakeS3Cluster) Endpoint() string { return f.endpoint }
func (f *fakeS3Cluster) Region() string   { return f.region }

func (f *fakeS3Cluster) key(bucket, k string) string { return bucket + "/" + k }

func (f *fakeS3Cluster) plant(bucket, k string, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.store[f.key(bucket, k)] = append([]byte(nil), body...)
}

func (f *fakeS3Cluster) Get(_ context.Context, bucket, k string) (io.ReadCloser, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	if f.getErr != nil {
		return nil, 0, f.getErr
	}
	body, ok := f.store[f.key(bucket, k)]
	if !ok {
		return nil, 0, errors.New("not found")
	}
	cp := append([]byte(nil), body...)
	return io.NopCloser(bytes.NewReader(cp)), int64(len(cp)), nil
}

func (f *fakeS3Cluster) Put(_ context.Context, bucket, k string, body io.Reader, _ int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.puts++
	if f.putErr != nil {
		return f.putErr
	}
	buf, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	f.store[f.key(bucket, k)] = buf
	return nil
}

func (f *fakeS3Cluster) Copy(_ context.Context, srcBucket, srcKey, dstBucket, dstKey string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.copies++
	if f.copyErr != nil {
		return f.copyErr
	}
	src, ok := f.store[f.key(srcBucket, srcKey)]
	if !ok {
		return errors.New("not found")
	}
	f.store[f.key(dstBucket, dstKey)] = append([]byte(nil), src...)
	return nil
}

func seedS3Object(t *testing.T, m meta.Store, src *fakeS3Cluster, bucketID uuid.UUID, key, srcBucket, backendKey string, body []byte, cluster string) *meta.Object {
	t.Helper()
	src.plant(srcBucket, backendKey, body)
	if err := m.PutObject(context.Background(), &meta.Object{
		BucketID:     bucketID,
		Key:          key,
		Size:         int64(len(body)),
		ETag:         "deadbeef",
		StorageClass: "STANDARD",
		Mtime:        time.Now().UTC(),
		IsLatest:     true,
		Manifest: &data.Manifest{
			Class: "STANDARD",
			Size:  int64(len(body)),
			BackendRef: &data.BackendRef{
				Backend: "s3",
				Key:     backendKey,
				Size:    int64(len(body)),
				Cluster: cluster,
			},
		},
	}, false); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	obj, err := m.GetObject(context.Background(), bucketID, key, "")
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	return obj
}

func mintKeyFixed(key string) func([16]byte) string {
	return func([16]byte) string { return key }
}

func TestS3MoverHappyPathGetPut(t *testing.T) {
	m := metamem.New()
	b, err := m.CreateBucket(context.Background(), "s3bkt", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	// Endpoints differ → Get+Put path.
	src := newFakeS3Cluster("c1", "http://src.example.com", "us-east-1")
	tgt := newFakeS3Cluster("c2", "http://tgt.example.com", "us-west-1")
	body := bytes.Repeat([]byte("x"), 4096)
	obj := seedS3Object(t, m, src, b.ID, "obj", "bkt-c1", "uuid-key-1", body, "c1")

	plan := []Move{{
		Bucket:      b.Name,
		BucketID:    b.ID,
		ObjectKey:   obj.Key,
		VersionID:   obj.VersionID,
		ChunkIdx:    0,
		FromCluster: "c1",
		ToCluster:   "c2",
		SrcRef:      data.ChunkRef{Cluster: "c1", OID: "uuid-key-1", Size: int64(len(body))},
		Class:       "STANDARD",
	}}

	metrics := newFakeMetrics()
	mover := &S3Mover{
		Clusters: map[string]S3Cluster{"c1": src, "c2": tgt},
		BucketBy: func(_, cluster string) string {
			return map[string]string{"c1": "bkt-c1", "c2": "bkt-c2"}[cluster]
		},
		Meta:     m,
		Region:   "default",
		Logger:   slog.Default(),
		Metrics:  metrics,
		Inflight: 4,
		KeyMint:  mintKeyFixed("new-key-1"),
	}
	if err := mover.Move(context.Background(), plan); err != nil {
		t.Fatalf("Move: %v", err)
	}

	if src.copies != 0 {
		t.Errorf("expected zero server-side copies on cross-endpoint path, got %d", src.copies)
	}
	if src.gets != 1 {
		t.Errorf("src gets: got %d want 1", src.gets)
	}
	if tgt.puts != 1 {
		t.Errorf("tgt puts: got %d want 1", tgt.puts)
	}

	post, err := m.GetObject(context.Background(), b.ID, "obj", "")
	if err != nil {
		t.Fatalf("post GetObject: %v", err)
	}
	if post.Manifest.BackendRef == nil {
		t.Fatalf("post BackendRef nil")
	}
	if post.Manifest.BackendRef.Cluster != "c2" {
		t.Errorf("post cluster: got %q want c2", post.Manifest.BackendRef.Cluster)
	}
	if post.Manifest.BackendRef.Key != "new-key-1" {
		t.Errorf("post key: got %q want new-key-1", post.Manifest.BackendRef.Key)
	}

	entries, err := m.ListGCEntries(context.Background(), "default", time.Now().Add(time.Hour), 100)
	if err != nil {
		t.Fatalf("ListGCEntries: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("gc entries: got %d want 1", len(entries))
	}
	if entries[0].Chunk.Cluster != "c1" || entries[0].Chunk.OID != "uuid-key-1" || entries[0].Chunk.Pool != "bkt-c1" {
		t.Errorf("gc entry shape mismatch: %+v", entries[0].Chunk)
	}

	if got := metrics.bytes["c1->c2"]; got != int64(len(body)) {
		t.Errorf("bytes_moved: got %d want %d", got, len(body))
	}
	if got := metrics.chunks["c1->c2:s3bkt"]; got != 1 {
		t.Errorf("chunks_moved: got %d want 1", got)
	}
	if len(metrics.confs) != 0 {
		t.Errorf("cas conflicts: %v", metrics.confs)
	}
}

func TestS3MoverHappyPathServerSideCopy(t *testing.T) {
	m := metamem.New()
	b, err := m.CreateBucket(context.Background(), "s3bkt", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	// Same endpoint+region → Copy path (one logical S3 endpoint, two
	// operator-labelled clusters routing to different buckets).
	src := newFakeS3Cluster("c1", "http://shared.example.com", "us-east-1")
	tgt := newFakeS3Cluster("c2", "http://shared.example.com", "us-east-1")
	body := bytes.Repeat([]byte("y"), 1024)
	obj := seedS3Object(t, m, src, b.ID, "obj", "bkt-shared", "uuid-src", body, "c1")

	plan := []Move{{
		Bucket:      b.Name,
		BucketID:    b.ID,
		ObjectKey:   obj.Key,
		VersionID:   obj.VersionID,
		ChunkIdx:    0,
		FromCluster: "c1",
		ToCluster:   "c2",
		SrcRef:      data.ChunkRef{Cluster: "c1", OID: "uuid-src", Size: int64(len(body))},
		Class:       "STANDARD",
	}}

	mover := &S3Mover{
		Clusters: map[string]S3Cluster{"c1": src, "c2": tgt},
		BucketBy: func(_, cluster string) string {
			return map[string]string{"c1": "bkt-shared", "c2": "bkt-shared"}[cluster]
		},
		Meta:     m,
		Logger:   slog.Default(),
		Metrics:  newFakeMetrics(),
		Inflight: 1,
		KeyMint:  mintKeyFixed("new-server-copy-key"),
	}
	if err := mover.Move(context.Background(), plan); err != nil {
		t.Fatalf("Move: %v", err)
	}
	if src.copies != 1 {
		t.Errorf("server-side copy: got %d want 1", src.copies)
	}
	if src.gets != 0 || tgt.puts != 0 {
		t.Errorf("expected no Get/Put on same-endpoint path; gets=%d puts=%d", src.gets, tgt.puts)
	}
}

func TestS3MoverCASConflictDiscardsNewObject(t *testing.T) {
	m := metamem.New()
	b, err := m.CreateBucket(context.Background(), "cas", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	src := newFakeS3Cluster("c1", "http://src.example.com", "us-east-1")
	tgt := newFakeS3Cluster("c2", "http://tgt.example.com", "us-west-1")
	body := []byte("body")
	obj := seedS3Object(t, m, src, b.ID, "x", "bkt-c1", "uuid-old", body, "c1")

	// Race: rewrite the live manifest's BackendRef.Key out from under
	// the planned src — the mover should detect this in
	// buildUpdatedBackendManifest (ok=false) and treat as CAS conflict.
	mutated := *obj.Manifest
	br := *obj.Manifest.BackendRef
	br.Key = "raced-key"
	mutated.BackendRef = &br
	if applied, err := m.SetObjectStorage(context.Background(), b.ID, "x", obj.VersionID, "STANDARD", "STANDARD", &mutated); err != nil || !applied {
		t.Fatalf("SetObjectStorage seed: applied=%v err=%v", applied, err)
	}

	plan := []Move{{
		Bucket:      b.Name,
		BucketID:    b.ID,
		ObjectKey:   obj.Key,
		VersionID:   obj.VersionID,
		ChunkIdx:    0,
		FromCluster: "c1",
		ToCluster:   "c2",
		SrcRef:      data.ChunkRef{Cluster: "c1", OID: "uuid-old", Size: int64(len(body))},
		Class:       "STANDARD",
	}}

	metrics := newFakeMetrics()
	mover := &S3Mover{
		Clusters: map[string]S3Cluster{"c1": src, "c2": tgt},
		BucketBy: func(_, cluster string) string {
			return map[string]string{"c1": "bkt-c1", "c2": "bkt-c2"}[cluster]
		},
		Meta:     m,
		Logger:   slog.Default(),
		Metrics:  metrics,
		Inflight: 1,
		KeyMint:  mintKeyFixed("new-conflict-key"),
	}
	if err := mover.Move(context.Background(), plan); err != nil {
		t.Fatalf("Move: %v", err)
	}

	post, err := m.GetObject(context.Background(), b.ID, "x", "")
	if err != nil {
		t.Fatalf("post GetObject: %v", err)
	}
	if post.Manifest.BackendRef.Key != "raced-key" {
		t.Errorf("live manifest unexpectedly rewritten by mover: %q", post.Manifest.BackendRef.Key)
	}
	if metrics.confs["cas"] != 1 {
		t.Errorf("cas conflicts: got %d want 1", metrics.confs["cas"])
	}
	if metrics.chunks["c1->c2:cas"] != 0 {
		t.Errorf("chunks_moved on conflict: got %d want 0", metrics.chunks["c1->c2:cas"])
	}

	// The freshly-written target key should be in the GC queue (c2-side).
	entries, err := m.ListGCEntries(context.Background(), "default", time.Now().Add(time.Hour), 100)
	if err != nil {
		t.Fatalf("ListGCEntries: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("gc entries: got %d want 1", len(entries))
	}
	if entries[0].Chunk.Cluster != "c2" || entries[0].Chunk.OID != "new-conflict-key" {
		t.Errorf("conflict gc entry should reference target; got %+v", entries[0].Chunk)
	}
}

func TestS3MoverUnknownClusterDrops(t *testing.T) {
	m := metamem.New()
	b, err := m.CreateBucket(context.Background(), "drop", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	src := newFakeS3Cluster("c1", "http://src.example.com", "us-east-1")
	obj := seedS3Object(t, m, src, b.ID, "ob", "bkt-c1", "uuid-old", []byte("z"), "c1")
	mover := &S3Mover{
		Clusters: map[string]S3Cluster{"c1": src},
		Meta:     m,
		Logger:   slog.Default(),
		Metrics:  newFakeMetrics(),
		Inflight: 1,
	}
	plan := []Move{{
		Bucket:      b.Name,
		BucketID:    b.ID,
		ObjectKey:   obj.Key,
		VersionID:   obj.VersionID,
		ChunkIdx:    0,
		FromCluster: "c1",
		ToCluster:   "missing",
		SrcRef:      data.ChunkRef{Cluster: "c1", OID: "uuid-old", Size: 1},
		Class:       "STANDARD",
	}}
	if err := mover.Move(context.Background(), plan); err != nil {
		t.Fatalf("Move: %v", err)
	}
	post, _ := m.GetObject(context.Background(), b.ID, "ob", "")
	if post.Manifest.BackendRef.Cluster != "c1" {
		t.Errorf("manifest should not have moved on unknown target; got %q", post.Manifest.BackendRef.Cluster)
	}
}

func TestS3MoverOwnsReportsConfiguredClusters(t *testing.T) {
	mv := &S3Mover{Clusters: map[string]S3Cluster{"a": newFakeS3Cluster("a", "", "")}}
	if !mv.Owns("a") {
		t.Error("expected Owns(a)==true")
	}
	if mv.Owns("b") {
		t.Error("expected Owns(b)==false")
	}
}

func TestS3MoverRequiresMeta(t *testing.T) {
	mv := &S3Mover{Clusters: map[string]S3Cluster{"a": newFakeS3Cluster("a", "", "")}}
	err := mv.Move(context.Background(), []Move{{ToCluster: "a", BucketID: uuid.New()}})
	if err == nil {
		t.Fatal("expected error when Meta nil")
	}
}

func TestS3MoverWithoutBucketResolverDrops(t *testing.T) {
	// When the BucketResolver returns "" for the target cluster the
	// mover refuses to copy (no target bucket → no destination).
	m := metamem.New()
	b, err := m.CreateBucket(context.Background(), "nores", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	src := newFakeS3Cluster("c1", "http://src.example.com", "us-east-1")
	tgt := newFakeS3Cluster("c2", "http://tgt.example.com", "us-west-1")
	body := []byte("hello")
	obj := seedS3Object(t, m, src, b.ID, "obj", "bkt-c1", "old", body, "c1")
	mover := &S3Mover{
		Clusters: map[string]S3Cluster{"c1": src, "c2": tgt},
		// Resolver only knows c1; c2 returns empty.
		BucketBy: func(_, cluster string) string {
			if cluster == "c1" {
				return "bkt-c1"
			}
			return ""
		},
		Meta:     m,
		Logger:   slog.Default(),
		Metrics:  newFakeMetrics(),
		Inflight: 1,
	}
	plan := []Move{{
		Bucket:      b.Name,
		BucketID:    b.ID,
		ObjectKey:   obj.Key,
		VersionID:   obj.VersionID,
		ChunkIdx:    0,
		FromCluster: "c1",
		ToCluster:   "c2",
		SrcRef:      data.ChunkRef{Cluster: "c1", OID: "old", Size: int64(len(body))},
		Class:       "STANDARD",
	}}
	if err := mover.Move(context.Background(), plan); err != nil {
		t.Fatalf("Move: %v", err)
	}
	if src.gets != 0 || tgt.puts != 0 {
		t.Errorf("expected no Get/Put when no target bucket resolves; gets=%d puts=%d", src.gets, tgt.puts)
	}
	post, _ := m.GetObject(context.Background(), b.ID, "obj", "")
	if post.Manifest.BackendRef.Cluster != "c1" {
		t.Errorf("manifest should not have moved; got cluster %q", post.Manifest.BackendRef.Cluster)
	}
}
