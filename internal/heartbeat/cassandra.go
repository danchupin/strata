package heartbeat

import (
	"context"
	"time"

	"github.com/gocql/gocql"
)

// CassandraStore persists heartbeats in the cluster_nodes table. The table
// DDL (with default_time_to_live = 30) lives in
// internal/meta/cassandra/schema.go so the same bootstrap path that creates
// the rest of the keyspace creates this table too.
type CassandraStore struct {
	S *gocql.Session
}

const cassandraTTLSeconds = int(DefaultTTL / time.Second)

func (c *CassandraStore) WriteHeartbeat(ctx context.Context, n Node) error {
	workers := nilToEmpty(n.Workers)
	leaderFor := nilToEmpty(n.LeaderFor)
	return c.S.Query(`
		INSERT INTO cluster_nodes (node_id, address, version, started_at, workers, leader_for, last_heartbeat)
		VALUES (?, ?, ?, ?, ?, ?, ?) USING TTL ?
	`,
		n.ID, n.Address, n.Version, n.StartedAt, workers, leaderFor, n.LastHeartbeat, cassandraTTLSeconds,
	).WithContext(ctx).Exec()
}

func (c *CassandraStore) ListNodes(ctx context.Context) ([]Node, error) {
	iter := c.S.Query(`
		SELECT node_id, address, version, started_at, workers, leader_for, last_heartbeat
		FROM cluster_nodes
	`).WithContext(ctx).Iter()

	var (
		out []Node
		n   Node
	)
	for iter.Scan(&n.ID, &n.Address, &n.Version, &n.StartedAt, &n.Workers, &n.LeaderFor, &n.LastHeartbeat) {
		out = append(out, n)
		n = Node{}
	}
	if err := iter.Close(); err != nil {
		return nil, err
	}
	return out, nil
}

// gocql encodes a nil set<text> as a tombstone, which races with TTL eviction
// and silently drops the row in some cluster topologies. Pass an empty slice
// instead.
func nilToEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
