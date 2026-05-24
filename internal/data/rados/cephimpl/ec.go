package cephimpl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/data/rados"
)

// ClusterECCapability implements data.ClusterECCapability.
func (b *Backend) ClusterECCapability(ctx context.Context, clusterID string) (bool, int, int, error) {
	if b == nil {
		return false, 0, 0, errors.New("rados backend closed")
	}
	if clusterID == "" {
		clusterID = rados.DefaultCluster
	}
	if _, ok := b.clusters[clusterID]; !ok {
		return false, 0, 0, data.ErrClusterUnknown
	}
	pool, ns := b.seedPoolForCluster(clusterID)
	if pool == "" {
		return false, 0, 0, fmt.Errorf("rados: cluster %q has no configured pool", clusterID)
	}
	if _, err := b.ioctx(ctx, clusterID, pool, ns); err != nil {
		return false, 0, 0, fmt.Errorf("rados: open ioctx on %s/%s: %w", clusterID, pool, err)
	}
	b.mu.Lock()
	p, ok := b.pools[clusterID]
	b.mu.Unlock()
	if !ok {
		return false, 0, 0, fmt.Errorf("rados: no conn pool for cluster %q", clusterID)
	}
	conn := p.Next()
	if conn == nil {
		return false, 0, 0, fmt.Errorf("rados: empty conn pool for cluster %q", clusterID)
	}

	args, err := json.Marshal(map[string]string{
		"prefix": "osd pool get",
		"pool":   pool,
		"var":    "erasure_code_profile",
		"format": "json",
	})
	if err != nil {
		return false, 0, 0, fmt.Errorf("rados: marshal pool-get args: %w", err)
	}
	out, _, err := conn.MonCommand(args)
	if err != nil {
		return false, 0, 0, nil
	}
	var poolGet struct {
		Profile string `json:"erasure_code_profile"`
	}
	if err := json.Unmarshal(out, &poolGet); err != nil || poolGet.Profile == "" {
		return false, 0, 0, nil
	}

	args2, err := json.Marshal(map[string]string{
		"prefix": "osd erasure-code-profile get",
		"name":   poolGet.Profile,
		"format": "json",
	})
	if err != nil {
		return false, 0, 0, fmt.Errorf("rados: marshal profile-get args: %w", err)
	}
	out2, _, err := conn.MonCommand(args2)
	if err != nil {
		return false, 0, 0, fmt.Errorf("rados: profile-get %q: %w", poolGet.Profile, err)
	}
	var profile map[string]string
	if err := json.Unmarshal(out2, &profile); err != nil {
		return false, 0, 0, fmt.Errorf("rados: profile-get parse: %w", err)
	}
	k, err := strconv.Atoi(profile["k"])
	if err != nil || k <= 0 {
		return false, 0, 0, fmt.Errorf("rados: profile %q missing k: %v", poolGet.Profile, profile["k"])
	}
	m, err := strconv.Atoi(profile["m"])
	if err != nil || m <= 0 {
		return false, 0, 0, fmt.Errorf("rados: profile %q missing m: %v", poolGet.Profile, profile["m"])
	}
	return true, k, m, nil
}

func (b *Backend) seedPoolForCluster(clusterID string) (pool, ns string) {
	for _, spec := range b.classes {
		c := spec.Cluster
		if c == "" {
			c = rados.DefaultCluster
		}
		if c == clusterID && spec.Pool != "" {
			return spec.Pool, spec.Namespace
		}
	}
	for _, spec := range b.classes {
		if spec.Pool != "" {
			return spec.Pool, spec.Namespace
		}
	}
	return "", ""
}

var _ data.ClusterECCapability = (*Backend)(nil)
