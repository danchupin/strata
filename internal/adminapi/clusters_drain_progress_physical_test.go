package adminapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/data/placement"
	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/metrics"
	"github.com/danchupin/strata/internal/rebalance"
)

// physicalProbeBackend is a test double satisfying data.Backend +
// data.ClusterStatsProbe + data.ClusterObjectCountProbe. statsErr /
// objectErr force a probe error so the per-probe counter increments and
// the response surfaces null for the failed leg.
type physicalProbeBackend struct {
	bytes     int64
	objects   int64
	statsErr  error
	objectErr error
}

func (p *physicalProbeBackend) PutChunks(ctx context.Context, r io.Reader, class string) (*data.Manifest, error) {
	return nil, errors.New("not implemented")
}
func (p *physicalProbeBackend) GetChunks(ctx context.Context, m *data.Manifest, offset, length int64) (io.ReadCloser, error) {
	return nil, errors.New("not implemented")
}
func (p *physicalProbeBackend) Delete(ctx context.Context, m *data.Manifest) error { return nil }
func (p *physicalProbeBackend) Close() error                                        { return nil }
func (p *physicalProbeBackend) ClusterStats(ctx context.Context, clusterID string) (int64, int64, error) {
	if p.statsErr != nil {
		return 0, 0, p.statsErr
	}
	return p.bytes, 0, nil
}
func (p *physicalProbeBackend) ClusterObjectCount(ctx context.Context, clusterID string) (int64, error) {
	if p.objectErr != nil {
		return 0, p.objectErr
	}
	return p.objects, nil
}

func seedDrainingScan(t *testing.T, s *Server, id string, migratable int64, bytes int64) {
	t.Helper()
	if err := s.Meta.SetClusterState(context.Background(), id, meta.ClusterStateEvacuating, meta.ClusterModeEvacuate, 0); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	s.RebalanceProgress.CommitScan(0, []string{id}, map[string]rebalance.ScanResult{
		id: {MigratableChunks: migratable, Bytes: bytes},
	}, time.Now().UTC())
}

// TestClusterDrainProgress_PhysicalFieldsPopulated wires a probe-capable
// backend + a fresh cluster-stats cache. The response must surface the
// physical_chunks_on_cluster + physical_bytes_on_cluster pair plus the
// explicit gc_queue_pending counter (US-001 drain-progress-physical).
func TestClusterDrainProgress_PhysicalFieldsPopulated(t *testing.T) {
	s := newTestServer()
	s.KnownClusters = map[string]struct{}{"c1": {}}
	s.RebalanceProgress = rebalance.NewProgressTracker(time.Minute)
	s.ClusterStatsCache = placement.NewClusterStatsCache(10 * time.Second)
	s.Data = &physicalProbeBackend{bytes: 1024 * 1024, objects: 7}
	seedDrainingScan(t, s, "c1", 5, 5*1024)

	rr := putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/clusters/c1/drain-progress", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", rr.Code, rr.Body.String())
	}
	var got ClusterDrainProgressResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.PhysicalChunksOnCluster == nil || *got.PhysicalChunksOnCluster != 7 {
		t.Fatalf("PhysicalChunksOnCluster: got %v want 7", got.PhysicalChunksOnCluster)
	}
	if got.PhysicalBytesOnCluster == nil || *got.PhysicalBytesOnCluster != 1024*1024 {
		t.Fatalf("PhysicalBytesOnCluster: got %v want 1048576", got.PhysicalBytesOnCluster)
	}
	if got.GCQueuePending != 0 {
		t.Fatalf("GCQueuePending: got %d want 0", got.GCQueuePending)
	}
}

// TestClusterDrainProgress_NullPhysicalOnMemoryBackend asserts the JSON
// shape for backends that don't satisfy either probe interface — the
// physical_* fields must be JSON null (pointer nil), and gc_queue_pending
// is still surfaced as an explicit 0.
func TestClusterDrainProgress_NullPhysicalOnMemoryBackend(t *testing.T) {
	s := newTestServer()
	s.KnownClusters = map[string]struct{}{"c1": {}}
	s.RebalanceProgress = rebalance.NewProgressTracker(time.Minute)
	seedDrainingScan(t, s, "c1", 5, 5*1024)

	rr := putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/clusters/c1/drain-progress", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", rr.Code, rr.Body.String())
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rr.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw: %v", err)
	}
	if string(raw["physical_chunks_on_cluster"]) != "null" {
		t.Errorf("physical_chunks_on_cluster: got %s want null", raw["physical_chunks_on_cluster"])
	}
	if string(raw["physical_bytes_on_cluster"]) != "null" {
		t.Errorf("physical_bytes_on_cluster: got %s want null", raw["physical_bytes_on_cluster"])
	}
	if string(raw["gc_queue_pending"]) != "0" {
		t.Errorf("gc_queue_pending: got %s want 0", raw["gc_queue_pending"])
	}
}

// TestClusterDrainProgress_ProbeErrorIncrementsCounter forces a stats
// probe failure; the response must succeed with null physical_bytes_on_
// cluster and the per-probe counter must bump.
func TestClusterDrainProgress_ProbeErrorIncrementsCounter(t *testing.T) {
	s := newTestServer()
	s.KnownClusters = map[string]struct{}{"c1": {}}
	s.RebalanceProgress = rebalance.NewProgressTracker(time.Minute)
	s.ClusterStatsCache = placement.NewClusterStatsCache(10 * time.Second)
	s.Data = &physicalProbeBackend{
		objects:  3,
		statsErr: errors.New("ceph df boom"),
	}
	seedDrainingScan(t, s, "c1", 5, 5*1024)

	before := readCounter(t, "c1", "stats")
	rr := putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/clusters/c1/drain-progress", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", rr.Code, rr.Body.String())
	}
	after := readCounter(t, "c1", "stats")
	if after-before != 1 {
		t.Fatalf("strata_drain_progress_probe_errors_total{cluster=c1,probe=stats} delta: got %v want 1", after-before)
	}
	var got ClusterDrainProgressResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.PhysicalBytesOnCluster != nil {
		t.Errorf("PhysicalBytesOnCluster: got %v want nil (probe errored)", got.PhysicalBytesOnCluster)
	}
	if got.PhysicalChunksOnCluster == nil || *got.PhysicalChunksOnCluster != 3 {
		t.Errorf("PhysicalChunksOnCluster: got %v want 3 (object-count probe succeeded)", got.PhysicalChunksOnCluster)
	}
	foundWarning := false
	for _, w := range got.Warnings {
		if w == "cluster-stats probe failed: ceph df boom" {
			foundWarning = true
		}
	}
	if !foundWarning {
		t.Errorf("expected stats-probe warning, got %v", got.Warnings)
	}
}

// TestClusterDrainProgress_PhysicalCacheHit makes the first poll wire
// real values into the cache, then forces a second poll under a stats
// probe that would error if reached. The second poll must satisfy
// entirely from cache → no error counter bump, fresh JSON values.
func TestClusterDrainProgress_PhysicalCacheHit(t *testing.T) {
	s := newTestServer()
	s.KnownClusters = map[string]struct{}{"c1": {}}
	s.RebalanceProgress = rebalance.NewProgressTracker(time.Minute)
	s.ClusterStatsCache = placement.NewClusterStatsCache(10 * time.Second)
	backend := &physicalProbeBackend{bytes: 2048, objects: 9}
	s.Data = backend
	seedDrainingScan(t, s, "c1", 5, 5*1024)

	// First poll fills cache.
	rr := putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/clusters/c1/drain-progress", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", rr.Code, rr.Body.String())
	}
	// Force any subsequent probe to error — if cache works, this must
	// stay unreached.
	backend.statsErr = errors.New("never reached")
	backend.objectErr = errors.New("never reached either")
	before := readCounter(t, "c1", "stats")
	rr = putAdmin(t, s, "alice", http.MethodGet, "/admin/v1/clusters/c1/drain-progress", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("second poll: got %d body=%s", rr.Code, rr.Body.String())
	}
	if delta := readCounter(t, "c1", "stats") - before; delta != 0 {
		t.Fatalf("cache-hit poll incremented stats counter by %v", delta)
	}
	var got ClusterDrainProgressResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.PhysicalBytesOnCluster == nil || *got.PhysicalBytesOnCluster != 2048 {
		t.Errorf("PhysicalBytesOnCluster: got %v want 2048 (cache hit)", got.PhysicalBytesOnCluster)
	}
	if got.PhysicalChunksOnCluster == nil || *got.PhysicalChunksOnCluster != 9 {
		t.Errorf("PhysicalChunksOnCluster: got %v want 9 (cache hit)", got.PhysicalChunksOnCluster)
	}
}

// readCounter pulls one `strata_drain_progress_probe_errors_total` cell
// out of the global prometheus registry. Returns 0 when the labelset has
// never been observed.
func readCounter(t *testing.T, cluster, probe string) float64 {
	t.Helper()
	c := metrics.DrainProgressProbeErrorsTotal.WithLabelValues(cluster, probe)
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatalf("collect counter: %v", err)
	}
	if m.Counter == nil {
		t.Fatalf("metric is not a counter")
	}
	return m.Counter.GetValue()
}
