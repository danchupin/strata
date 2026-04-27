package rados

import (
	"fmt"
	"strings"
)

// ClusterSpec is the per-RADOS-cluster connection config. ID is the operator
// label (e.g. "default", "cold-eu") referenced by ClassSpec.Cluster. ConfigFile
// is the path to the cluster's ceph.conf; Keyring overrides the cephx keyring
// path; User is the cephx client name (defaults to "admin" at dial time).
type ClusterSpec struct {
	ID         string
	ConfigFile string
	Keyring    string
	User       string
}

// ParseClusters parses a STRATA_RADOS_CLUSTERS string.
//
// Format: <id>:<conf-path>:<keyring-path>, comma-separated. The keyring path is
// optional (empty after the second colon means "use the keyring from
// conf-path"). The id and conf-path are required.
//
// Example:
//
//	"default:/etc/ceph/ceph.conf:/etc/ceph/ceph.client.admin.keyring,
//	 cold-eu:/etc/ceph/cold-eu.conf:/etc/ceph/cold-eu.keyring"
func ParseClusters(s string) (map[string]ClusterSpec, error) {
	out := make(map[string]ClusterSpec)
	if strings.TrimSpace(s) == "" {
		return out, nil
	}
	for _, entry := range strings.Split(s, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		parts := strings.SplitN(entry, ":", 3)
		if len(parts) < 2 {
			return nil, fmt.Errorf("rados clusters: entry %q must be <id>:<conf>[:<keyring>]", entry)
		}
		id := strings.TrimSpace(parts[0])
		conf := strings.TrimSpace(parts[1])
		if id == "" {
			return nil, fmt.Errorf("rados clusters: entry %q has empty cluster id", entry)
		}
		if conf == "" {
			return nil, fmt.Errorf("rados clusters: entry %q has empty config path", entry)
		}
		if _, dup := out[id]; dup {
			return nil, fmt.Errorf("rados clusters: duplicate cluster id %q", id)
		}
		spec := ClusterSpec{ID: id, ConfigFile: conf}
		if len(parts) == 3 {
			spec.Keyring = strings.TrimSpace(parts[2])
		}
		out[id] = spec
	}
	return out, nil
}

// BuildClusters resolves the cluster map for a Backend from a Config.
//
// Precedence:
//  1. cfg.Clusters (operator-supplied multi-cluster map) wins as-is.
//  2. Otherwise the legacy single-cluster fields (cfg.ConfigFile / cfg.User /
//     cfg.Keyring) materialise as a single entry under DefaultCluster.
//  3. The two paths can also coexist: a multi-cluster map without a "default"
//     entry has the legacy single-cluster fields appended under DefaultCluster
//     so existing classes that omit "@cluster" stay routable.
//
// Returns at least one cluster or an error explaining what's missing.
func BuildClusters(cfg Config) (map[string]ClusterSpec, error) {
	clusters := make(map[string]ClusterSpec, len(cfg.Clusters)+1)
	for id, spec := range cfg.Clusters {
		spec.ID = id
		clusters[id] = spec
	}
	if _, hasDefault := clusters[DefaultCluster]; !hasDefault {
		if cfg.ConfigFile != "" || cfg.User != "" || cfg.Keyring != "" {
			clusters[DefaultCluster] = ClusterSpec{
				ID:         DefaultCluster,
				ConfigFile: cfg.ConfigFile,
				User:       cfg.User,
				Keyring:    cfg.Keyring,
			}
		}
	}
	if len(clusters) == 0 {
		return nil, fmt.Errorf("rados: no clusters configured (set Clusters or legacy ConfigFile)")
	}
	return clusters, nil
}

// ValidateClusterRefs checks every Class' Cluster field resolves to a real
// entry in the cluster map. A class that omits Cluster (empty string) is
// treated as DefaultCluster — must still be present.
//
// Pulled out of New() so the rule can be unit-tested without librados.
func ValidateClusterRefs(classes map[string]ClassSpec, clusters map[string]ClusterSpec) error {
	for name, spec := range classes {
		id := spec.Cluster
		if id == "" {
			id = DefaultCluster
		}
		if _, ok := clusters[id]; !ok {
			return fmt.Errorf("rados: class %q references unknown cluster %q", name, id)
		}
	}
	return nil
}
