//go:build ceph

package rados

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

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

	type poolKey struct{ cluster, pool, ns string }
	classByPool := make(map[poolKey][]string)
	for class, spec := range b.classes {
		cluster := spec.Cluster
		if cluster == "" {
			cluster = DefaultCluster
		}
		k := poolKey{cluster: cluster, pool: spec.Pool, ns: spec.Namespace}
		classByPool[k] = append(classByPool[k], class)
	}

	pools := make([]data.PoolStatus, 0, len(classByPool))
	keys := make([]poolKey, 0, len(classByPool))
	for k := range classByPool {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].cluster != keys[j].cluster {
			return keys[i].cluster < keys[j].cluster
		}
		if keys[i].pool != keys[j].pool {
			return keys[i].pool < keys[j].pool
		}
		return keys[i].ns < keys[j].ns
	})

	var warnings []string
	clusters := make(map[string]struct{})
	for _, k := range keys {
		clusters[k.cluster] = struct{}{}
		classes := append([]string(nil), classByPool[k]...)
		sort.Strings(classes)
		ps := data.PoolStatus{
			Name:  k.pool,
			Class: strings.Join(classes, ","),
		}
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
		ps.ObjectCount = stat.Num_objects
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
