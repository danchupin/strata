package adminapi

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sort"
	"time"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/metrics"
	"github.com/danchupin/strata/internal/promclient"
	"github.com/danchupin/strata/internal/rebalance"
	"github.com/danchupin/strata/internal/s3api"
)

// categoryRank assigns the on-wire sort priority for ByBucket entries.
// Stuck rows surface first so the UI's stuck-buckets drawer can paginate
// them without re-sorting; migratable rows trail because they need no
// operator action.
func categoryRank(c string) int {
	switch c {
	case "stuck_single_policy":
		return 0
	case "stuck_no_policy":
		return 1
	case "migratable":
		return 2
	default:
		return 3
	}
}

func sortedByBucket(in map[string]rebalance.BucketScanCategory) []BucketDrainProgressEntry {
	if len(in) == 0 {
		return nil
	}
	out := make([]BucketDrainProgressEntry, 0, len(in))
	for name, b := range in {
		out = append(out, BucketDrainProgressEntry{
			Name:       name,
			Category:   b.Category,
			ChunkCount: b.ChunkCount,
			BytesUsed:  b.BytesUsed,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		ri, rj := categoryRank(out[i].Category), categoryRank(out[j].Category)
		if ri != rj {
			return ri < rj
		}
		if out[i].ChunkCount != out[j].ChunkCount {
			return out[i].ChunkCount > out[j].ChunkCount
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// PromQL backing the ETA half of GET /admin/v1/clusters/{id}/drain-progress.
// `from=<id>` is the source-cluster label stamped by the rebalance worker
// on every successful chunk copy (US-005 placement-rebalance), so the
// expression measures chunks-leaving-the-cluster — exactly the rate that
// drains it.
const drainMoveRateExprFmt = `sum(rate(strata_rebalance_chunks_moved_total{from="%s"}[5m]))`

// ClusterDrainProgressResponse is the wire shape returned by
// GET /admin/v1/clusters/{id}/drain-progress (US-003 drain-lifecycle,
// extended in US-002 drain-transparency with categorized counters,
// US-006 drain-transparency with per-bucket breakdown, US-006
// drain-cleanup with not_ready_reasons).
//
// MigratableChunks / StuckSinglePolicyChunks / StuckNoPolicyChunks are
// the three category counters; ChunksOnCluster is their sum (kept for
// dashboard compatibility). ETASeconds is computed against migratable
// chunks only — stuck chunks never converge to zero through migration
// alone. DeregisterReady fires only when ALL THREE conditions hold:
// total_chunks==0, gc_queue_pending==0, no_open_multipart==0. Any unmet
// condition is surfaced via NotReadyReasons so the UI's amber chip can
// explain why dereg is gated. ByBucket is the per-bucket breakdown
// straight off the tracker's last committed scan.
//
// Pointer fields go *null* in the JSON output when no value applies.
//
// PhysicalChunksOnCluster / PhysicalBytesOnCluster are the physical RADOS
// pool snapshot (US-001 drain-progress-physical). null when the data
// backend does not implement the corresponding probe (memory + s3) OR
// the probe failed on this poll (the per-probe counter
// `strata_drain_progress_probe_errors_total` bumps in that case).
// GCQueuePending is the explicit length of the cluster-scoped GC queue,
// surfaced so the UI's `Awaiting GC` state can render the
// physical-but-not-yet-deleted backlog independent of
// `not_ready_reasons`.
type ClusterDrainProgressResponse struct {
	State                   string                     `json:"state"`
	Mode                    string                     `json:"mode"`
	Weight                  int                        `json:"weight"`
	ChunksOnCluster         *int64                     `json:"chunks_on_cluster"`
	MigratableChunks        *int64                     `json:"migratable_chunks"`
	StuckSinglePolicyChunks *int64                     `json:"stuck_single_policy_chunks"`
	StuckNoPolicyChunks     *int64                     `json:"stuck_no_policy_chunks"`
	BytesOnCluster          *int64                     `json:"bytes_on_cluster"`
	PhysicalChunksOnCluster *int64                     `json:"physical_chunks_on_cluster"`
	PhysicalBytesOnCluster  *int64                     `json:"physical_bytes_on_cluster"`
	GCQueuePending          int                        `json:"gc_queue_pending"`
	BaseChunks              *int64                     `json:"base_chunks_at_start"`
	LastScanAt              *string                    `json:"last_scan_at"`
	ETASeconds              *int64                     `json:"eta_seconds"`
	DeregisterReady         *bool                      `json:"deregister_ready"`
	NotReadyReasons         []string                   `json:"not_ready_reasons,omitempty"`
	ByBucket                []BucketDrainProgressEntry `json:"by_bucket,omitempty"`
	Warnings                []string                   `json:"warnings,omitempty"`
}

// Drain-progress not-ready reason codes (US-006 drain-cleanup). The wire
// vocabulary is fixed so UI consumers can map each token to a localized
// chip message without parsing free-form prose.
const (
	DrainNotReadyChunksRemaining = "chunks_remaining"
	DrainNotReadyGCQueuePending  = "gc_queue_pending"
	DrainNotReadyOpenMultipart   = "open_multipart"
)

// BucketDrainProgressEntry is one bucket's contribution to the per-
// cluster drain-progress snapshot (US-006 drain-transparency). Category
// matches the rebalance worker's classifier — "migratable",
// "stuck_single_policy", or "stuck_no_policy". Sorted on the wire:
// stuck_single_policy first, then stuck_no_policy, then migratable;
// within each category descending by chunk_count then ascending by name.
type BucketDrainProgressEntry struct {
	Name       string `json:"name"`
	Category   string `json:"category"`
	ChunkCount int64  `json:"chunk_count"`
	BytesUsed  int64  `json:"bytes_used"`
}

// handleClusterDrainProgress serves GET /admin/v1/clusters/{id}/drain-
// progress. Reads from the in-process ProgressTracker populated by the
// rebalance worker — never scans synchronously per request. ETA is
// derived from the Prom rate of `strata_rebalance_chunks_moved_total
// {from=<id>}` over the last 5 minutes; Prom unavailable or rate=0
// surfaces eta_seconds=null without failing the request.
func (s *Server) handleClusterDrainProgress(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "cluster id is required")
		return
	}
	if len(s.KnownClusters) > 0 {
		if _, ok := s.KnownClusters[id]; !ok {
			writeJSONError(w, http.StatusBadRequest, "UnknownCluster",
				"cluster id is not configured (check STRATA_RADOS_CLUSTERS / STRATA_S3_CLUSTERS)")
			return
		}
	}
	if s.Meta == nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", "meta store not configured")
		return
	}
	if s.RebalanceProgress == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "ProgressUnavailable",
			"drain-progress requires the rebalance worker — start `strata server --workers=rebalance`")
		return
	}

	ctx := r.Context()
	owner := auth.FromContext(ctx).Owner
	s3api.SetAuditOverride(ctx, "admin:GetClusterDrainProgress", "cluster:"+id, "-", owner)

	rows, err := s.Meta.ListClusterStates(ctx)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	row, ok := rows[id]
	if !ok || row.State == "" {
		row = meta.ClusterStateRow{State: meta.ClusterStateLive}
	}
	resp := ClusterDrainProgressResponse{State: row.State, Mode: row.Mode, Weight: row.Weight}
	if !meta.IsDrainingForWrite(row.State) {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	// US-002: stop-writes mode runs no migration scan — surface the
	// state/mode + an explanatory warning so the UI does not render
	// the "pending" placeholder forever.
	if row.State == meta.ClusterStateDrainingReadonly {
		resp.Warnings = append(resp.Warnings, "stop-writes mode — migration scan skipped; undrain or upgrade to evacuate")
		writeJSON(w, http.StatusOK, resp)
		return
	}

	snap, scanOK := s.RebalanceProgress.Snapshot(id)
	region := s.Region
	if region == "" {
		region = "default"
	}
	gcPending, gcErr := s.Meta.ListChunkDeletionsByCluster(ctx, region, id, 1)
	if gcErr != nil {
		// Treat the probe as best-effort; an opaque error must not bury
		// the rest of the snapshot, but we surface it as a warning so
		// operators can investigate. deregister_ready stays gated
		// (gcPending=0 falls through to "pending" semantics below).
		gcPending = 0
	}
	// gcQueuePending mirrors the existing safety-gate count but stays
	// surfaced even when the gc probe errors so the UI can render
	// `Awaiting GC` independently of `not_ready_reasons` shape.
	gcQueuePending := gcPending
	mpPending, mpErr := s.Meta.ListMultipartUploadsByCluster(ctx, id, 1)
	if mpErr != nil {
		mpPending = 0
	}
	physicalBytes, physicalObjects, physWarnings := s.probeClusterPhysical(ctx, id)
	resp = buildDrainProgressResponse(ctx, s.Prom, id, row, snap, scanOK, s.RebalanceProgress.Interval, time.Now(), gcPending, mpPending, gcQueuePending, physicalBytes, physicalObjects)
	if gcErr != nil {
		resp.Warnings = append(resp.Warnings, "gc-queue probe failed: "+gcErr.Error())
	}
	if mpErr != nil {
		resp.Warnings = append(resp.Warnings, "multipart probe failed: "+mpErr.Error())
	}
	resp.Warnings = append(resp.Warnings, physWarnings...)
	writeJSON(w, http.StatusOK, resp)
}

// probeClusterPhysical resolves the (physicalBytes, physicalObjects) pair
// for the cluster id, going through the 10 s ClusterStatsCache first
// (US-001 drain-progress-physical). Cache miss / expired triggers one
// ClusterStats + ClusterObjectCount probe round; per-probe errors bump
// `strata_drain_progress_probe_errors_total{cluster,probe}` and surface
// nil pointers (UI fallback to manifest count). Backends that don't
// implement either probe interface return nil/nil — no error path.
//
// Returns the two pointer fields plus any operator-facing warnings.
func (s *Server) probeClusterPhysical(ctx context.Context, id string) (*int64, *int64, []string) {
	statsProbe, hasStats := s.Data.(data.ClusterStatsProbe)
	countProbe, hasCount := s.Data.(data.ClusterObjectCountProbe)
	if !hasStats && !hasCount {
		return nil, nil, nil
	}
	if bytes, objects, ok := s.ClusterStatsCache.Get(id); ok {
		var bp, op *int64
		if hasStats {
			b := bytes
			bp = &b
		}
		if hasCount {
			o := objects
			op = &o
		}
		return bp, op, nil
	}
	var (
		warnings        []string
		bytesPtr        *int64
		objectsPtr      *int64
		gotBytes        int64
		gotObjects      int64
		bytesOK         bool
		objectsOK       bool
	)
	if hasStats {
		used, _, err := statsProbe.ClusterStats(ctx, id)
		if err != nil {
			metrics.DrainProgressProbeErrorsTotal.WithLabelValues(id, "stats").Inc()
			warnings = append(warnings, "cluster-stats probe failed: "+err.Error())
		} else {
			gotBytes = used
			bytesOK = true
			b := used
			bytesPtr = &b
		}
	}
	if hasCount {
		objects, err := countProbe.ClusterObjectCount(ctx, id)
		if err != nil {
			metrics.DrainProgressProbeErrorsTotal.WithLabelValues(id, "object_count").Inc()
			warnings = append(warnings, "cluster-object-count probe failed: "+err.Error())
		} else {
			gotObjects = objects
			objectsOK = true
			o := objects
			objectsPtr = &o
		}
	}
	// Cache only when both probes (those that exist) succeeded — partial
	// success would poison the cache with stale zero on the failed leg
	// for the next 10 s.
	cacheable := (!hasStats || bytesOK) && (!hasCount || objectsOK)
	if cacheable {
		s.ClusterStatsCache.Set(id, gotBytes, gotObjects)
	}
	return bytesPtr, objectsPtr, warnings
}


// buildDrainProgressResponse is the testable core of the handler. Pure
// over its inputs — no IO besides the optional Prom query. gcPending
// and mpPending are the cluster-scoped counts from
// Meta.ListChunkDeletionsByCluster / Meta.ListMultipartUploadsByCluster
// — together with the per-snapshot chunk total they form the three
// safety conditions of `deregister_ready` (US-006 drain-cleanup).
//
// gcQueuePending is surfaced verbatim on the wire as the explicit
// `gc_queue_pending` counter (US-001 drain-progress-physical). Callers
// pass the same value as gcPending; the parameter is split to make the
// observability vs safety roles independent at the type level.
// physicalBytes / physicalObjects are the resolved pointer pair from
// probeClusterPhysical — nil on backends without the corresponding
// probe interface, nil on per-probe failure.
func buildDrainProgressResponse(ctx context.Context, prom *promclient.Client, id string, row meta.ClusterStateRow, snap rebalance.ProgressSnapshot, ok bool, interval time.Duration, now time.Time, gcPending, mpPending, gcQueuePending int, physicalBytes, physicalObjects *int64) ClusterDrainProgressResponse {
	out := ClusterDrainProgressResponse{State: row.State, Mode: row.Mode, Weight: row.Weight}
	out.GCQueuePending = gcQueuePending
	out.PhysicalBytesOnCluster = physicalBytes
	out.PhysicalChunksOnCluster = physicalObjects
	if !ok || snap.LastScanAt.IsZero() {
		out.Warnings = append(out.Warnings, "progress scan pending; rebalance worker has not yet committed a tick")
		return out
	}
	total := snap.Chunks()
	bytes := snap.Bytes
	out.ChunksOnCluster = &total
	migratable := snap.MigratableChunks
	out.MigratableChunks = &migratable
	stuckSingle := snap.StuckSinglePolicyChunks
	out.StuckSinglePolicyChunks = &stuckSingle
	stuckNo := snap.StuckNoPolicyChunks
	out.StuckNoPolicyChunks = &stuckNo
	out.BytesOnCluster = &bytes
	if snap.BaseChunks > 0 {
		base := snap.BaseChunks
		out.BaseChunks = &base
	}
	scan := snap.LastScanAt.UTC().Format(time.RFC3339)
	out.LastScanAt = &scan
	var reasons []string
	if total > 0 {
		reasons = append(reasons, DrainNotReadyChunksRemaining)
	}
	if gcPending > 0 {
		reasons = append(reasons, DrainNotReadyGCQueuePending)
	}
	if mpPending > 0 {
		reasons = append(reasons, DrainNotReadyOpenMultipart)
	}
	deregReady := len(reasons) == 0
	out.DeregisterReady = &deregReady
	out.NotReadyReasons = reasons
	out.ByBucket = sortedByBucket(snap.ByBucket)

	// ETA is meaningful only for migratable chunks — stuck chunks need an
	// operator policy edit, not migration throughput.
	if eta, ok := drainETASeconds(ctx, prom, id, snap.MigratableChunks); ok {
		out.ETASeconds = &eta
	}

	if interval > 0 && now.Sub(snap.LastScanAt) > 2*interval {
		out.Warnings = append(out.Warnings, "progress data stale")
	}
	return out
}

// drainETASeconds queries Prom for the chunk-out rate and returns the
// projected seconds-to-empty. Returns (0, false) on any Prom error, on
// rate=0 (no traffic → undefined ETA), or when chunks is already 0
// (the dereg chip carries the signal, ETA stays null).
func drainETASeconds(ctx context.Context, prom *promclient.Client, id string, chunks int64) (int64, bool) {
	if prom == nil || !prom.Available() || chunks <= 0 {
		return 0, false
	}
	samples, err := prom.Query(ctx, fmt.Sprintf(drainMoveRateExprFmt, id))
	if err != nil || len(samples) == 0 {
		return 0, false
	}
	rate := samples[0].Value
	if math.IsNaN(rate) || math.IsInf(rate, 0) || rate <= 0 {
		return 0, false
	}
	eta := float64(chunks) / rate
	if math.IsNaN(eta) || math.IsInf(eta, 0) || eta <= 0 {
		return 0, false
	}
	return int64(math.Ceil(eta)), true
}
