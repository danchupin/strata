package heartbeat

import (
	"context"
	"testing"
	"time"
)

func TestMemoryStoreRoundTrip(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()

	now := time.Now().UTC()
	if err := s.WriteHeartbeat(ctx, Node{
		ID: "a", Address: "10.0.0.1:9000", Version: "v1",
		StartedAt: now.Add(-time.Hour), LastHeartbeat: now,
	}); err != nil {
		t.Fatalf("write a: %v", err)
	}
	if err := s.WriteHeartbeat(ctx, Node{
		ID: "b", Address: "10.0.0.2:9000", Version: "v1",
		StartedAt: now.Add(-time.Minute), LastHeartbeat: now,
	}); err != nil {
		t.Fatalf("write b: %v", err)
	}

	nodes, err := s.ListNodes(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(nodes) != 2 {
		t.Fatalf("len(nodes)=%d want 2", len(nodes))
	}
	if nodes[0].ID != "a" || nodes[1].ID != "b" {
		t.Errorf("sort order: %v", []string{nodes[0].ID, nodes[1].ID})
	}
}

func TestMemoryStoreEvictsExpired(t *testing.T) {
	s := NewMemoryStore()
	s.TTL = 50 * time.Millisecond
	ctx := context.Background()

	if err := s.WriteHeartbeat(ctx, Node{
		ID:            "stale",
		LastHeartbeat: time.Now().UTC().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("write: %v", err)
	}

	nodes, err := s.ListNodes(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(nodes) != 0 {
		t.Errorf("expected stale node evicted, got %d", len(nodes))
	}
}

func TestHeartbeaterWritesOnRun(t *testing.T) {
	s := NewMemoryStore()
	hb := &Heartbeater{
		Store: s,
		Node: Node{
			ID:        "node-1",
			Address:   ":9000",
			Version:   "test",
			StartedAt: time.Now().UTC(),
		},
		Interval: 5 * time.Millisecond,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	hb.Run(ctx)

	nodes, err := s.ListNodes(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(nodes) != 1 || nodes[0].ID != "node-1" {
		t.Fatalf("nodes=%+v", nodes)
	}
	if nodes[0].LastHeartbeat.IsZero() {
		t.Error("LastHeartbeat zero")
	}
}

func TestSetLeaderForAddRemove(t *testing.T) {
	hb := &Heartbeater{Node: Node{ID: "n1"}}

	hb.SetLeaderFor("gc", true)
	hb.SetLeaderFor("lifecycle", true)
	hb.SetLeaderFor("gc", true) // idempotent re-acquire
	if got, want := hb.Node.LeaderFor, []string{"gc", "lifecycle"}; !equalSlice(got, want) {
		t.Fatalf("after acquires, LeaderFor=%v want %v", got, want)
	}

	hb.SetLeaderFor("notify", false) // release something never owned
	hb.SetLeaderFor("gc", false)
	if got, want := hb.Node.LeaderFor, []string{"lifecycle"}; !equalSlice(got, want) {
		t.Fatalf("after release, LeaderFor=%v want %v", got, want)
	}
}

func TestRunPicksUpLeaderForOnTick(t *testing.T) {
	s := NewMemoryStore()
	hb := &Heartbeater{
		Store:    s,
		Node:     Node{ID: "n1", Address: ":9000", Version: "v", StartedAt: time.Now().UTC()},
		Interval: 5 * time.Millisecond,
	}
	hb.SetLeaderFor("gc", true)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	hb.Run(ctx)

	nodes, err := s.ListNodes(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("nodes=%d", len(nodes))
	}
	if got, want := nodes[0].LeaderFor, []string{"gc"}; !equalSlice(got, want) {
		t.Fatalf("first run LeaderFor=%v want %v", got, want)
	}

	hb.SetLeaderFor("gc", false)
	hb.SetLeaderFor("lifecycle", true)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel2()
	hb.Run(ctx2)

	nodes, err = s.ListNodes(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if got, want := nodes[0].LeaderFor, []string{"lifecycle"}; !equalSlice(got, want) {
		t.Fatalf("second run LeaderFor=%v want %v", got, want)
	}
}

func equalSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestIsAlive(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		n    Node
		ttl  time.Duration
		want bool
	}{
		{"fresh", Node{LastHeartbeat: now}, time.Minute, true},
		{"stale", Node{LastHeartbeat: now.Add(-2 * time.Minute)}, time.Minute, false},
		{"zero", Node{}, time.Minute, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsAlive(tc.n, tc.ttl); got != tc.want {
				t.Errorf("got %v want %v", got, tc.want)
			}
		})
	}
}
