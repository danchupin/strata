//go:build ceph

package rados

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/danchupin/strata/internal/data"
)

// radosCheckCap is the maximum number of MonCommand `status` check
// summaries appended to DataHealthReport.Warnings per cluster. The wire
// payload stays small while still giving the operator enough fingerprint
// to drill into HEALTH_WARN/HEALTH_ERR via `ceph status` directly.
const radosCheckCap = 5

// DataHealth implements data.HealthProbe (US-002 web-ui-storage-status).
// Walks the configured classes map, groups by unique (cluster, pool, ns),
// reports per-pool stats via IOContext.GetPoolStats(), and folds the
// cluster-wide MonCommand `status` JSON's HEALTH_WARN/HEALTH_ERR checks
// into Warnings (up to radosCheckCap entries per cluster).
//
// Failure-isolated: a single pool / cluster error degrades just that row
// and adds a warning; the rest of the report still renders so the storage
// page can show partial state instead of bouncing the operator off a 502.
func (b *Backend) DataHealth(ctx context.Context) (*data.DataHealthReport, error) {
	if b == nil {
		return nil, errors.New("rados backend closed")
	}

	pending := buildPendingPoolStatuses(b.classes, b.clusters)
	pools := make([]data.PoolStatus, 0, len(pending))
	var warnings []string
	clusters := make(map[string]struct{})
	for _, p := range pending {
		k := p.group
		ps := p.status
		clusters[k.cluster] = struct{}{}
		ioctx, err := b.ioctx(ctx, k.cluster, k.pool, k.ns)
		if err != nil {
			ps.State = "error"
			warnings = append(warnings, fmt.Sprintf("pool %s/%s: %v", k.cluster, k.pool, err))
			pools = append(pools, ps)
			continue
		}
		stat, err := ioctx.GetPoolStats()
		if err != nil {
			ps.State = "error"
			warnings = append(warnings, fmt.Sprintf("pool %s/%s: stats: %v", k.cluster, k.pool, err))
			pools = append(pools, ps)
			continue
		}
		// PRD: BytesUsed = Num_kb * 1024 (Num_bytes is unreliable on some
		// older Ceph builds; Num_kb is the documented stable contract).
		ps.BytesUsed = stat.Num_kb * 1024
		ps.ChunkCount = stat.Num_objects
		ps.State = "ok"
		pools = append(pools, ps)
	}

	clusterNames := make([]string, 0, len(clusters))
	for c := range clusters {
		clusterNames = append(clusterNames, c)
	}
	sort.Strings(clusterNames)
	for _, cluster := range clusterNames {
		warnings = append(warnings, b.clusterStatusWarnings(ctx, cluster)...)
	}

	return &data.DataHealthReport{
		Backend:  "rados",
		Pools:    pools,
		Warnings: warnings,
	}, nil
}

// clusterStatusWarnings runs `ceph status --format json` via MonCommand on
// the per-cluster Conn (lazily dialed by the earlier ioctx() loop) and
// returns up to radosCheckCap warning lines. HEALTH_OK returns an empty
// slice.
func (b *Backend) clusterStatusWarnings(ctx context.Context, cluster string) []string {
	b.mu.Lock()
	conn, ok := b.conns[cluster]
	b.mu.Unlock()
	if !ok {
		return nil
	}
	args, err := json.Marshal(map[string]string{"prefix": "status", "format": "json"})
	if err != nil {
		return []string{fmt.Sprintf("cluster %s: status args: %v", cluster, err)}
	}
	out, _, err := conn.MonCommand(args)
	if err != nil {
		return []string{fmt.Sprintf("cluster %s: status: %v", cluster, err)}
	}
	var st cephStatus
	if err := json.Unmarshal(out, &st); err != nil {
		return []string{fmt.Sprintf("cluster %s: status parse: %v", cluster, err)}
	}
	if st.Health.Status == "" || st.Health.Status == "HEALTH_OK" {
		return nil
	}
	out2 := []string{fmt.Sprintf("cluster %s: %s", cluster, st.Health.Status)}
	codes := make([]string, 0, len(st.Health.Checks))
	for c := range st.Health.Checks {
		codes = append(codes, c)
	}
	sort.Strings(codes)
	for i, code := range codes {
		if i >= radosCheckCap {
			break
		}
		summary := st.Health.Checks[code].Summary.Message
		if summary == "" {
			summary = code
		}
		out2 = append(out2, fmt.Sprintf("cluster %s: %s: %s", cluster, code, summary))
	}
	return out2
}

type cephStatus struct {
	Health struct {
		Status string               `json:"status"`
		Checks map[string]cephCheck `json:"checks"`
	} `json:"health"`
}

type cephCheck struct {
	Severity string `json:"severity"`
	Summary  struct {
		Message string `json:"message"`
	} `json:"summary"`
}

var _ data.HealthProbe = (*Backend)(nil)
var _ data.ClusterStatsProbe = (*Backend)(nil)
var _ data.ClusterObjectCountProbe = (*Backend)(nil)

// ClusterObjectCount implements data.ClusterObjectCountProbe (US-001
// drain-progress-physical). Walks the configured classes map, filters by
// clusterID, opens an ioctx per unique (cluster, pool, namespace), and
// sums Num_objects across pools via the existing GetPoolStats path
// (mirrors DataHealth so we reuse the same code path proven against
// pool-stat field-name churn). Returns (0, err) on any per-pool failure
// — the caller surfaces null, not a partial sum.
func (b *Backend) ClusterObjectCount(ctx context.Context, clusterID string) (int64, error) {
	if b == nil {
		return 0, errors.New("rados backend closed")
	}
	if clusterID == "" {
		clusterID = DefaultCluster
	}
	if _, ok := b.clusters[clusterID]; !ok {
		return 0, fmt.Errorf("rados: unknown cluster %q", clusterID)
	}
	type poolKey struct{ pool, ns string }
	// First pass: collect pools from classes that explicitly target this
	// cluster. This is the canonical path when the operator's
	// STRATA_RADOS_CLASSES env names per-cluster classes (e.g.
	// STANDARD@cephb=cephb:strata.rgw.buckets.data).
	targeted := make(map[poolKey]struct{})
	allPools := make(map[poolKey]struct{})
	for _, spec := range b.classes {
		c := spec.Cluster
		if c == "" {
			c = DefaultCluster
		}
		k := poolKey{pool: spec.Pool, ns: spec.Namespace}
		allPools[k] = struct{}{}
		if c == clusterID {
			targeted[k] = struct{}{}
		}
	}
	// Fallback: lab + most prod setups use uniform pool layout across
	// clusters (ceph-bootstrap creates the same pool names on every
	// cluster). When no class targets clusterID, query all known pool
	// names against this cluster directly — ioctx() will surface a
	// clear error if a pool genuinely doesn't exist on the target.
	pools := targeted
	if len(pools) == 0 {
		pools = allPools
	}
	if len(pools) == 0 {
		return 0, nil // no class config at all → nothing to count
	}
	var total int64
	for k := range pools {
		ioctx, err := b.ioctx(ctx, clusterID, k.pool, k.ns)
		if err != nil {
			return 0, fmt.Errorf("rados: open ioctx %s/%s: %w", clusterID, k.pool, err)
		}
		stat, err := ioctx.GetPoolStats()
		if err != nil {
			return 0, fmt.Errorf("rados: pool stats %s/%s: %w", clusterID, k.pool, err)
		}
		total += int64(stat.Num_objects)
	}
	return total, nil
}

// ClusterStats implements data.ClusterStatsProbe (US-006 placement-rebalance).
// Runs `ceph df --format json` via MonCommand on the per-cluster Conn and
// returns total_used_bytes / total_bytes from the `stats` block. The
// rebalance worker uses this to refuse moves into clusters above the
// configured fill ceiling.
func (b *Backend) ClusterStats(ctx context.Context, clusterID string) (int64, int64, error) {
	if b == nil {
		return 0, 0, errors.New("rados backend closed")
	}
	if clusterID == "" {
		clusterID = DefaultCluster
	}
	if _, ok := b.clusters[clusterID]; !ok {
		return 0, 0, fmt.Errorf("rados: unknown cluster %q", clusterID)
	}
	// Force a Conn dial via a per-cluster ioctx against any pool we know
	// about — MonCommand needs an open Conn but does not care which pool.
	// First pass: class targeted at this cluster. Fallback: any class's
	// pool name (lab + most prod setups have uniform pool layout across
	// clusters via ceph-bootstrap). ioctx() surfaces a clear error if a
	// pool genuinely doesn't exist on the target cluster.
	var seedPool, seedNS string
	for _, spec := range b.classes {
		c := spec.Cluster
		if c == "" {
			c = DefaultCluster
		}
		if c == clusterID {
			seedPool = spec.Pool
			seedNS = spec.Namespace
			break
		}
	}
	if seedPool == "" {
		// No class targets this cluster. Pick any class's pool name as
		// the seed — MonCommand still works once Conn is dialed (open
		// ioctx against a pool that exists on the target cluster).
		for _, spec := range b.classes {
			if spec.Pool != "" {
				seedPool = spec.Pool
				seedNS = spec.Namespace
				break
			}
		}
	}
	if seedPool == "" {
		return 0, 0, fmt.Errorf("rados: cluster %q has no configured class and no fallback pool", clusterID)
	}
	if _, err := b.ioctx(ctx, clusterID, seedPool, seedNS); err != nil {
		return 0, 0, fmt.Errorf("rados: open ioctx on %s/%s: %w", clusterID, seedPool, err)
	}
	b.mu.Lock()
	conn, ok := b.conns[clusterID]
	b.mu.Unlock()
	if !ok {
		return 0, 0, fmt.Errorf("rados: no conn cached for cluster %q", clusterID)
	}
	args, err := json.Marshal(map[string]string{"prefix": "df", "format": "json"})
	if err != nil {
		return 0, 0, fmt.Errorf("rados: marshal df args: %w", err)
	}
	out, _, err := conn.MonCommand(args)
	if err != nil {
		return 0, 0, fmt.Errorf("rados: df mon command: %w", err)
	}
	var df cephDF
	if err := json.Unmarshal(out, &df); err != nil {
		return 0, 0, fmt.Errorf("rados: df parse: %w", err)
	}
	// Sum bytes-used over strata-managed pools only (those registered in
	// `b.classes`). The cluster-wide `stats.total_used_bytes` includes
	// ceph internal pools like `.mgr` (always >0 even on empty clusters)
	// which is misleading to the drain-progress operator UX: physical
	// bytes should reflect *strata's* footprint on the cluster, not
	// ceph internals. totalBytes stays cluster-wide capacity (rebalance
	// worker fill-check semantic: "how full is the cluster overall").
	strataPools := make(map[string]struct{})
	for _, spec := range b.classes {
		if spec.Pool != "" {
			strataPools[spec.Pool] = struct{}{}
		}
	}
	var strataUsedBytes int64
	if len(strataPools) > 0 {
		for _, p := range df.Pools {
			if _, ok := strataPools[p.Name]; ok {
				strataUsedBytes += p.Stats.BytesUsed
			}
		}
	} else {
		// No classes registered → fall back to cluster-wide
		// total_used_bytes so the fill-check semantic still works on
		// degenerate setups.
		strataUsedBytes = df.Stats.TotalUsedBytes
	}
	return strataUsedBytes, df.Stats.TotalBytes, nil
}

type cephDF struct {
	Stats struct {
		TotalBytes     int64 `json:"total_bytes"`
		TotalUsedBytes int64 `json:"total_used_bytes"`
	} `json:"stats"`
	Pools []cephDFPool `json:"pools"`
}

type cephDFPool struct {
	Name  string `json:"name"`
	Stats struct {
		BytesUsed int64 `json:"bytes_used"`
	} `json:"stats"`
}
