package tikv

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/danchupin/strata/internal/heartbeat"
)

// NewHeartbeatStore returns a heartbeat.Store backed by the same TiKV
// connection as s. Heartbeat rows live under the s/hb/<nodeID> prefix and
// carry an ExpiresAt stamp in their payload — TiKV has no native TTL, so
// readers lazily skip expired rows and writers eager-delete a small batch
// per write to keep the prefix from leaking disk on long-running clusters.
func NewHeartbeatStore(s *Store) heartbeat.Store {
	return &heartbeatStore{kv: s.kv}
}

type heartbeatStore struct {
	kv kvBackend
}

// heartbeatRow is the JSON payload persisted under each s/hb/<nodeID> key.
// ExpiresAt is computed at write time so readers do not need to know the
// TTL value — they just compare against now().
type heartbeatRow struct {
	ID            string    `json:"id"`
	Address       string    `json:"address"`
	Version       string    `json:"version"`
	StartedAt     time.Time `json:"started_at"`
	Workers       []string  `json:"workers"`
	LeaderFor     []string  `json:"leader_for"`
	LastHeartbeat time.Time `json:"last_heartbeat"`
	ExpiresAt     time.Time `json:"expires_at"`
}

func heartbeatKey(nodeID string) []byte {
	out := []byte(prefixHeartbeat)
	return appendEscaped(out, nodeID)
}

func heartbeatPrefixRange() (start, end []byte) {
	start = []byte(prefixHeartbeat)
	end = append([]byte(nil), start...)
	end[len(end)-1]++ // s/hb0 — first byte after the prefix's trailing '/'
	return start, end
}

func (s *heartbeatStore) WriteHeartbeat(ctx context.Context, n heartbeat.Node) error {
	now := time.Now().UTC()
	last := n.LastHeartbeat
	if last.IsZero() {
		last = now
	}
	row := heartbeatRow{
		ID:            n.ID,
		Address:       n.Address,
		Version:       n.Version,
		StartedAt:     n.StartedAt.UTC(),
		Workers:       nilToEmpty(n.Workers),
		LeaderFor:     nilToEmpty(n.LeaderFor),
		LastHeartbeat: last.UTC(),
		ExpiresAt:     last.UTC().Add(heartbeat.DefaultTTL),
	}
	value, err := json.Marshal(&row)
	if err != nil {
		return err
	}

	txn, err := s.kv.Begin(ctx, false)
	if err != nil {
		return err
	}
	if err := txn.Set(heartbeatKey(n.ID), value); err != nil {
		_ = txn.Rollback()
		return err
	}
	// Lazy GC: scan the prefix once and delete the first few expired rows
	// inline. Cluster size is small (single-digit replicas in production),
	// so a full scan per write is cheap. Bounding the delete batch keeps
	// the txn from ballooning if a fleet of nodes departed at once.
	if err := s.gcExpiredLocked(ctx, txn, now, 16); err != nil {
		_ = txn.Rollback()
		return err
	}
	return txn.Commit(ctx)
}

func (s *heartbeatStore) ListNodes(ctx context.Context) ([]heartbeat.Node, error) {
	txn, err := s.kv.Begin(ctx, false)
	if err != nil {
		return nil, err
	}
	defer txn.Rollback() //nolint:errcheck // read-only txn

	start, end := heartbeatPrefixRange()
	pairs, err := txn.Scan(ctx, start, end, 0)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	out := make([]heartbeat.Node, 0, len(pairs))
	for _, p := range pairs {
		var row heartbeatRow
		if err := json.Unmarshal(p.Value, &row); err != nil {
			continue // skip corrupt rows; sweeper will overwrite next tick
		}
		if !row.ExpiresAt.IsZero() && now.After(row.ExpiresAt) {
			continue
		}
		out = append(out, heartbeat.Node{
			ID:            row.ID,
			Address:       row.Address,
			Version:       row.Version,
			StartedAt:     row.StartedAt,
			Workers:       row.Workers,
			LeaderFor:     row.LeaderFor,
			LastHeartbeat: row.LastHeartbeat,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// gcExpiredLocked deletes up to limit expired heartbeat rows visible to txn.
// Caller must Commit / Rollback. Best-effort: a partial scan failure leaves
// the txn intact for the heartbeat write to succeed.
func (s *heartbeatStore) gcExpiredLocked(ctx context.Context, txn kvTxn, now time.Time, limit int) error {
	start, end := heartbeatPrefixRange()
	pairs, err := txn.Scan(ctx, start, end, 0)
	if err != nil {
		return nil //nolint:nilerr // read failure should not block the write
	}
	deleted := 0
	for _, p := range pairs {
		if deleted >= limit {
			break
		}
		var row heartbeatRow
		if err := json.Unmarshal(p.Value, &row); err != nil {
			continue
		}
		if row.ExpiresAt.IsZero() || !now.After(row.ExpiresAt) {
			continue
		}
		if err := txn.Delete(p.Key); err != nil {
			return err
		}
		deleted++
	}
	return nil
}

func nilToEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
