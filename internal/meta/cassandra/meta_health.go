package cassandra

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/gocql/gocql"

	"github.com/danchupin/strata/internal/meta"
)

// metaHealthCacheTTL is the in-process cache window for MetaHealth — chosen
// so admin polling (30 s storage page, 30 s degraded banner, 60 s overview
// hero) hits the cache between renders instead of re-issuing system.peers /
// system.local queries on every adminapi hit.
const metaHealthCacheTTL = 10 * time.Second

// MetaHealth returns a snapshot of Cassandra peer topology by merging
// system.peers (every other node) with system.local (this node). CQL has no
// UNION ALL so the two queries are run sequentially at LOCAL_ONE consistency
// and merged Go-side. Schema-version drift across nodes is folded into
// Warnings. The result is cached in-process for metaHealthCacheTTL.
func (s *Store) MetaHealth(ctx context.Context) (*meta.MetaHealthReport, error) {
	s.metaHealthMu.Lock()
	if s.metaHealthCache != nil && time.Now().Before(s.metaHealthExpiry) {
		cached := *s.metaHealthCache
		s.metaHealthMu.Unlock()
		return &cached, nil
	}
	s.metaHealthMu.Unlock()

	if s == nil || s.s == nil || s.s.Closed() {
		return nil, errors.New("cassandra session closed")
	}

	nodes := make([]meta.NodeStatus, 0, 4)

	// system.local — the coordinator this client is connected to. Always one
	// row.
	{
		var (
			addr, dc, rack string
			schema         gocql.UUID
			release        string
		)
		iter := s.s.Query(
			`SELECT broadcast_address, data_center, rack, release_version, schema_version FROM system.local`,
		).WithContext(ctx).Consistency(gocql.LocalOne).Iter()
		for iter.Scan(&addr, &dc, &rack, &release, &schema) {
			nodes = append(nodes, meta.NodeStatus{
				Address:       addr,
				State:         "UP",
				SchemaVersion: schema.String(),
				DataCenter:    dc,
				Rack:          rack,
			})
		}
		if err := iter.Close(); err != nil {
			return nil, fmt.Errorf("system.local: %w", err)
		}
	}

	// system.peers — every other node the coordinator knows about.
	{
		var (
			peer, dc, rack string
			schema         gocql.UUID
			release        string
		)
		iter := s.s.Query(
			`SELECT peer, data_center, rack, release_version, schema_version FROM system.peers`,
		).WithContext(ctx).Consistency(gocql.LocalOne).Iter()
		for iter.Scan(&peer, &dc, &rack, &release, &schema) {
			nodes = append(nodes, meta.NodeStatus{
				Address:       peer,
				State:         "UP",
				SchemaVersion: schema.String(),
				DataCenter:    dc,
				Rack:          rack,
			})
		}
		if err := iter.Close(); err != nil {
			return nil, fmt.Errorf("system.peers: %w", err)
		}
	}

	rf := s.replicationFactor(ctx)

	warnings := schemaDriftWarnings(nodes)

	report := &meta.MetaHealthReport{
		Backend:           "cassandra",
		Nodes:             nodes,
		ReplicationFactor: rf,
		Warnings:          warnings,
	}

	s.metaHealthMu.Lock()
	s.metaHealthCache = report
	s.metaHealthExpiry = time.Now().Add(metaHealthCacheTTL)
	s.metaHealthMu.Unlock()
	return report, nil
}

// replicationFactor reads the replication map from system_schema.keyspaces
// for the active keyspace and returns the configured replication_factor.
// Returns 0 when the keyspace name is empty (bootstrap, dev rigs) or the
// query fails — callers treat 0 as "unknown".
func (s *Store) replicationFactor(ctx context.Context) int {
	if s.keyspace == "" {
		return 0
	}
	var repl map[string]string
	err := s.s.Query(
		`SELECT replication FROM system_schema.keyspaces WHERE keyspace_name=?`,
		s.keyspace,
	).WithContext(ctx).Consistency(gocql.LocalOne).Scan(&repl)
	if err != nil {
		return 0
	}
	// SimpleStrategy → replication_factor=N; NetworkTopologyStrategy →
	// per-DC RF, take the first numeric value.
	if rf, ok := repl["replication_factor"]; ok {
		if n, err := strconv.Atoi(rf); err == nil {
			return n
		}
	}
	for k, v := range repl {
		if k == "class" {
			continue
		}
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return 0
}

// schemaDriftWarnings returns a single warning string when more than one
// distinct schema_version is observed across the cluster — the canonical
// signal of a schema-agreement window or a stuck node. Empty slice when all
// nodes agree.
func schemaDriftWarnings(nodes []meta.NodeStatus) []string {
	versions := make(map[string]struct{}, 1)
	for _, n := range nodes {
		if n.SchemaVersion == "" {
			continue
		}
		versions[n.SchemaVersion] = struct{}{}
	}
	if len(versions) <= 1 {
		return nil
	}
	keys := make([]string, 0, len(versions))
	for v := range versions {
		keys = append(keys, v)
	}
	sort.Strings(keys)
	return []string{fmt.Sprintf("schema version drift across %d nodes: %v", len(nodes), keys)}
}

var _ meta.HealthProbe = (*Store)(nil)
