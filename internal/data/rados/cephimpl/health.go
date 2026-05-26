package cephimpl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/data/rados"
)

// radosCheckCap is the maximum number of MonCommand `status` check
// summaries appended to DataHealthReport.Warnings per cluster.
const radosCheckCap = 5

// DataHealth implements data.HealthProbe.
func (b *Backend) DataHealth(ctx context.Context) (*data.DataHealthReport, error) {
	if b == nil {
		return nil, errors.New("rados backend closed")
	}

	pending := rados.BuildPendingPoolStatuses(b.classes, b.clusters)
	pools := make([]data.PoolStatus, 0, len(pending))
	var warnings []string
	clusters := make(map[string]struct{})
	for _, p := range pending {
		clusters[p.Group.Cluster] = struct{}{}
		ps := p.Status
		ioctx, err := b.ioctx(ctx, p.Group.Cluster, p.Group.Pool, p.Group.NS)
		if err != nil {
			ps.State = "error"
			warnings = append(warnings, fmt.Sprintf("pool %s/%s: %v", p.Group.Cluster, p.Group.Pool, err))
			pools = append(pools, ps)
			continue
		}
		stat, err := ioctx.GetPoolStats()
		if err != nil {
			ps.State = "error"
			warnings = append(warnings, fmt.Sprintf("pool %s/%s: stats: %v", p.Group.Cluster, p.Group.Pool, err))
			pools = append(pools, ps)
			continue
		}
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

	// Per-cluster metrics (US-001 cycle B prod-observability). Aggregate
	// pool stats by cluster and emit one gauge sample per cluster — fired
	// once per DataHealth walk so the per-tick cost mirrors the existing
	// MonCommand fan-out. Pool rows with State=error are skipped so a
	// single broken pool does not zero out the cluster aggregate.
	if b.metrics != nil {
		objectsByCluster := map[string]uint64{}
		bytesByCluster := map[string]uint64{}
		for _, ps := range pools {
			if ps.State == "error" {
				continue
			}
			objectsByCluster[ps.Cluster] += ps.ChunkCount
			bytesByCluster[ps.Cluster] += ps.BytesUsed
		}
		for _, cluster := range clusterNames {
			b.metrics.SetClusterObjectCount(cluster, int64(objectsByCluster[cluster]))
			b.metrics.SetClusterBytesUsed(cluster, int64(bytesByCluster[cluster]))
		}
	}

	return &data.DataHealthReport{
		Backend:  "rados",
		Pools:    pools,
		Warnings: warnings,
	}, nil
}

func (b *Backend) clusterStatusWarnings(ctx context.Context, cluster string) []string {
	b.mu.Lock()
	p, ok := b.pools[cluster]
	b.mu.Unlock()
	if !ok {
		return nil
	}
	conn := p.Next()
	if conn == nil {
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

// ClusterObjectCount implements data.ClusterObjectCountProbe.
func (b *Backend) ClusterObjectCount(ctx context.Context, clusterID string) (int64, error) {
	if b == nil {
		return 0, errors.New("rados backend closed")
	}
	if clusterID == "" {
		clusterID = rados.DefaultCluster
	}
	if _, ok := b.clusters[clusterID]; !ok {
		return 0, fmt.Errorf("rados: unknown cluster %q", clusterID)
	}
	type poolKey struct{ pool, ns string }
	targeted := make(map[poolKey]struct{})
	allPools := make(map[poolKey]struct{})
	for _, spec := range b.classes {
		c := spec.Cluster
		if c == "" {
			c = rados.DefaultCluster
		}
		k := poolKey{pool: spec.Pool, ns: spec.Namespace}
		allPools[k] = struct{}{}
		if c == clusterID {
			targeted[k] = struct{}{}
		}
	}
	pools := targeted
	if len(pools) == 0 {
		pools = allPools
	}
	if len(pools) == 0 {
		return 0, nil
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

// ClusterStats implements data.ClusterStatsProbe.
func (b *Backend) ClusterStats(ctx context.Context, clusterID string) (int64, int64, error) {
	if b == nil {
		return 0, 0, errors.New("rados backend closed")
	}
	if clusterID == "" {
		clusterID = rados.DefaultCluster
	}
	if _, ok := b.clusters[clusterID]; !ok {
		return 0, 0, fmt.Errorf("rados: unknown cluster %q", clusterID)
	}
	var seedPool, seedNS string
	for _, spec := range b.classes {
		c := spec.Cluster
		if c == "" {
			c = rados.DefaultCluster
		}
		if c == clusterID {
			seedPool = spec.Pool
			seedNS = spec.Namespace
			break
		}
	}
	if seedPool == "" {
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
	p, ok := b.pools[clusterID]
	b.mu.Unlock()
	if !ok {
		return 0, 0, fmt.Errorf("rados: no conn pool for cluster %q", clusterID)
	}
	conn := p.Next()
	if conn == nil {
		return 0, 0, fmt.Errorf("rados: empty conn pool for cluster %q", clusterID)
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
