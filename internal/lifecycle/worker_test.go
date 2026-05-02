package lifecycle

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/meta/memory"
)

// multiClusterBackend simulates a multi-cluster RADOS deployment for tests.
// PutChunks routes by storage class to a (cluster, pool) pair; GetChunks +
// Delete dispatch by ChunkRef.Cluster, mirroring the real backend's contract.
type multiClusterBackend struct {
	classes map[string]struct {
		cluster string
		pool    string
	}
	mu      sync.Mutex
	chunks  map[string]map[string][]byte
	deletes map[string]int
}

func newMultiClusterBackend() *multiClusterBackend {
	return &multiClusterBackend{
		classes: map[string]struct {
			cluster string
			pool    string
		}{
			"STANDARD": {cluster: "default", pool: "hot"},
			"COLD":     {cluster: "cold-eu", pool: "cold-pool"},
		},
		chunks:  map[string]map[string][]byte{},
		deletes: map[string]int{},
	}
}

func (b *multiClusterBackend) PutChunks(ctx context.Context, r io.Reader, class string) (*data.Manifest, error) {
	if class == "" {
		class = "STANDARD"
	}
	spec, ok := b.classes[class]
	if !ok {
		return nil, fmt.Errorf("multiClusterBackend: unknown class %q", class)
	}
	body, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	h := md5.New()
	h.Write(body)
	oid := fmt.Sprintf("%s/%d", spec.cluster, time.Now().UnixNano())
	b.mu.Lock()
	if b.chunks[spec.cluster] == nil {
		b.chunks[spec.cluster] = map[string][]byte{}
	}
	b.chunks[spec.cluster][oid] = body
	b.mu.Unlock()
	return &data.Manifest{
		Class: class,
		Size:  int64(len(body)),
		ETag:  hex.EncodeToString(h.Sum(nil)),
		Chunks: []data.ChunkRef{{
			Cluster: spec.cluster,
			Pool:    spec.pool,
			OID:     oid,
			Size:    int64(len(body)),
		}},
	}, nil
}

func (b *multiClusterBackend) GetChunks(ctx context.Context, m *data.Manifest, off, length int64) (io.ReadCloser, error) {
	var bufs [][]byte
	b.mu.Lock()
	for _, c := range m.Chunks {
		cb := b.chunks[c.Cluster]
		if cb == nil {
			b.mu.Unlock()
			return nil, fmt.Errorf("no cluster %q", c.Cluster)
		}
		d, ok := cb[c.OID]
		if !ok {
			b.mu.Unlock()
			return nil, fmt.Errorf("missing chunk %s/%s", c.Cluster, c.OID)
		}
		bufs = append(bufs, append([]byte(nil), d...))
	}
	b.mu.Unlock()
	full := bytes.Join(bufs, nil)
	if off > int64(len(full)) {
		off = int64(len(full))
	}
	if length <= 0 || off+length > int64(len(full)) {
		length = int64(len(full)) - off
	}
	return io.NopCloser(bytes.NewReader(full[off : off+length])), nil
}

func (b *multiClusterBackend) Delete(ctx context.Context, m *data.Manifest) error {
	if m == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, c := range m.Chunks {
		if cb, ok := b.chunks[c.Cluster]; ok {
			delete(cb, c.OID)
		}
		b.deletes[c.Cluster]++
	}
	return nil
}

func (b *multiClusterBackend) Close() error { return nil }

func (b *multiClusterBackend) clusterChunkCount(cluster string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.chunks[cluster])
}

// TestLifecycleTransitionAcrossClusters drives a STANDARD-class object stored
// on the default cluster through a Transition rule that moves it to COLD on a
// remote cluster. Verifies that the new manifest's chunks resolve to the cold
// cluster and the old chunks land in the GC queue tagged with the default
// cluster id, exercising the full PutChunks-routing + per-cluster GC-enqueue
// path required by US-044.
func TestLifecycleTransitionAcrossClusters(t *testing.T) {
	ctx := context.Background()
	store := memory.New()
	be := newMultiClusterBackend()

	b, err := store.CreateBucket(ctx, "lc", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}

	payload := []byte("hello-multi-cluster")
	manifest, err := be.PutChunks(ctx, bytes.NewReader(payload), "STANDARD")
	if err != nil {
		t.Fatalf("PutChunks: %v", err)
	}
	obj := &meta.Object{
		BucketID:     b.ID,
		Key:          "k",
		Size:         int64(len(payload)),
		ETag:         manifest.ETag,
		StorageClass: "STANDARD",
		Mtime:        time.Now().Add(-2 * time.Hour),
		Manifest:     manifest,
	}
	if err := store.PutObject(ctx, obj, false); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	if be.clusterChunkCount("default") != 1 || be.clusterChunkCount("cold-eu") != 0 {
		t.Fatalf("after STANDARD PUT: default=%d cold-eu=%d want 1/0",
			be.clusterChunkCount("default"), be.clusterChunkCount("cold-eu"))
	}

	blob := []byte(`<LifecycleConfiguration><Rule><ID>r</ID><Status>Enabled</Status>
		<Filter><Prefix></Prefix></Filter>
		<Transition><Days>1</Days><StorageClass>COLD</StorageClass></Transition>
	</Rule></LifecycleConfiguration>`)
	if err := store.SetBucketLifecycle(ctx, b.ID, blob); err != nil {
		t.Fatalf("SetBucketLifecycle: %v", err)
	}

	w := &Worker{Meta: store, Data: be, Region: "default", AgeUnit: time.Hour, Logger: slog.Default()}
	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("lifecycle RunOnce: %v", err)
	}

	o, err := store.GetObject(ctx, b.ID, "k", "")
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	if o.StorageClass != "COLD" {
		t.Fatalf("storage class after transition: %q want COLD", o.StorageClass)
	}
	if len(o.Manifest.Chunks) != 1 {
		t.Fatalf("manifest chunks: %d want 1", len(o.Manifest.Chunks))
	}
	if o.Manifest.Chunks[0].Cluster != "cold-eu" {
		t.Errorf("new manifest cluster=%q want cold-eu", o.Manifest.Chunks[0].Cluster)
	}
	if be.clusterChunkCount("cold-eu") != 1 {
		t.Errorf("cold-eu cluster chunk count: got %d want 1", be.clusterChunkCount("cold-eu"))
	}

	entries, err := store.ListGCEntries(ctx, "default", time.Now().Add(time.Hour), 100)
	if err != nil {
		t.Fatalf("ListGCEntries: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("gc entries: %d want 1", len(entries))
	}
	if entries[0].Chunk.Cluster != "default" {
		t.Errorf("queued chunk cluster=%q want default", entries[0].Chunk.Cluster)
	}
}
