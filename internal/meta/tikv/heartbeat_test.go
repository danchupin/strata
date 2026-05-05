package tikv

import (
	"context"
	"testing"
	"time"

	"github.com/danchupin/strata/internal/heartbeat"
)

func TestHeartbeatStoreRoundTrip(t *testing.T) {
	s := openWithBackend(newMemBackend())
	hb := NewHeartbeatStore(s)
	ctx := context.Background()

	now := time.Now().UTC()
	if err := hb.WriteHeartbeat(ctx, heartbeat.Node{
		ID:            "alpha",
		Address:       "10.0.0.1:9000",
		Version:       "v1",
		StartedAt:     now.Add(-time.Hour),
		Workers:       []string{"gc", "lifecycle"},
		LeaderFor:     []string{"gc-leader"},
		LastHeartbeat: now,
	}); err != nil {
		t.Fatalf("write alpha: %v", err)
	}
	if err := hb.WriteHeartbeat(ctx, heartbeat.Node{
		ID:            "beta",
		Address:       "10.0.0.2:9000",
		LastHeartbeat: now,
	}); err != nil {
		t.Fatalf("write beta: %v", err)
	}

	nodes, err := hb.ListNodes(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(nodes) != 2 {
		t.Fatalf("len(nodes)=%d want 2", len(nodes))
	}
	if nodes[0].ID != "alpha" || nodes[1].ID != "beta" {
		t.Errorf("sort order: %v", []string{nodes[0].ID, nodes[1].ID})
	}
	if nodes[0].Address != "10.0.0.1:9000" || nodes[0].Version != "v1" {
		t.Errorf("alpha payload: %+v", nodes[0])
	}
	if len(nodes[0].Workers) != 2 || nodes[0].LeaderFor[0] != "gc-leader" {
		t.Errorf("alpha workers/leaderFor: %+v", nodes[0])
	}
}

func TestHeartbeatStoreOverwriteSameID(t *testing.T) {
	s := openWithBackend(newMemBackend())
	hb := NewHeartbeatStore(s)
	ctx := context.Background()

	now := time.Now().UTC()
	if err := hb.WriteHeartbeat(ctx, heartbeat.Node{
		ID: "node-1", Address: "old:9000", LastHeartbeat: now,
	}); err != nil {
		t.Fatalf("write 1: %v", err)
	}
	if err := hb.WriteHeartbeat(ctx, heartbeat.Node{
		ID: "node-1", Address: "new:9000", LastHeartbeat: now,
	}); err != nil {
		t.Fatalf("write 2: %v", err)
	}

	nodes, err := hb.ListNodes(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("len(nodes)=%d want 1 (overwrite expected)", len(nodes))
	}
	if nodes[0].Address != "new:9000" {
		t.Errorf("address=%q want new:9000", nodes[0].Address)
	}
}

func TestHeartbeatStoreFiltersExpiredOnRead(t *testing.T) {
	s := openWithBackend(newMemBackend())
	hb := NewHeartbeatStore(s)
	ctx := context.Background()

	stale := time.Now().UTC().Add(-time.Hour)
	if err := hb.WriteHeartbeat(ctx, heartbeat.Node{
		ID:            "stale",
		Address:       ":1",
		LastHeartbeat: stale,
	}); err != nil {
		t.Fatalf("write stale: %v", err)
	}
	fresh := time.Now().UTC()
	if err := hb.WriteHeartbeat(ctx, heartbeat.Node{
		ID:            "fresh",
		Address:       ":2",
		LastHeartbeat: fresh,
	}); err != nil {
		t.Fatalf("write fresh: %v", err)
	}

	nodes, err := hb.ListNodes(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(nodes) != 1 || nodes[0].ID != "fresh" {
		t.Fatalf("expected only fresh, got %+v", nodes)
	}
}

func TestHeartbeatStoreEagerGCDeletesExpired(t *testing.T) {
	s := openWithBackend(newMemBackend())
	hb := NewHeartbeatStore(s)
	ctx := context.Background()

	stale := time.Now().UTC().Add(-time.Hour)
	if err := hb.WriteHeartbeat(ctx, heartbeat.Node{
		ID:            "stale",
		LastHeartbeat: stale,
	}); err != nil {
		t.Fatalf("write stale: %v", err)
	}
	// Trigger eager GC by writing a fresh row.
	fresh := time.Now().UTC()
	if err := hb.WriteHeartbeat(ctx, heartbeat.Node{
		ID:            "fresh",
		LastHeartbeat: fresh,
	}); err != nil {
		t.Fatalf("write fresh: %v", err)
	}

	// Inspect the raw KV to confirm the stale row is physically gone.
	txn, err := s.kv.Begin(ctx, false)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer txn.Rollback()
	start, end := heartbeatPrefixRange()
	pairs, err := txn.Scan(ctx, start, end, 0)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(pairs) != 1 {
		t.Fatalf("len(pairs)=%d want 1 (stale should be eager-deleted)", len(pairs))
	}
}
