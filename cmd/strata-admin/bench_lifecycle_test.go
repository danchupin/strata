package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestApp_BenchLifecycle_MultiReplica pins the US-006 multi-replica shape:
// --replicas=N spawns N workers each pinned to ReplicaInfo=(N, i) racing for
// per-bucket leases on a shared in-process locker. With --buckets=9 the
// distribution gate hands every replica 3 buckets to expire.
func TestApp_BenchLifecycle_MultiReplica(t *testing.T) {
	t.Setenv("STRATA_META_BACKEND", "memory")
	t.Setenv("STRATA_DATA_BACKEND", "memory")

	var stdout, stderr bytes.Buffer
	a := newApp(&stdout, &stderr, []string{"bench-lifecycle", "--objects=90", "--concurrency=4", "--replicas=3", "--buckets=9"})
	if err := a.run(context.Background()); err != nil {
		t.Fatalf("bench-lifecycle: %v stderr=%s", err, stderr.String())
	}
	var res benchResult
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout.String())), &res); err != nil {
		t.Fatalf("decode: %v (raw=%q)", err, stdout.String())
	}
	if res.Bench != "lifecycle" {
		t.Errorf("bench=%q want lifecycle", res.Bench)
	}
	if res.Shards != 3 {
		t.Errorf("shards=%d want 3 (replicas)", res.Shards)
	}
	// objects/buckets = 90/9 = 10 per bucket * 9 buckets = 90 seeded
	if res.Entries != 90 {
		t.Errorf("entries=%d want 90", res.Entries)
	}
}

// TestApp_BenchLifecycle_RejectsBadReplicas: the [1, 16] clamp is enforced
// at the flag boundary.
func TestApp_BenchLifecycle_RejectsBadReplicas(t *testing.T) {
	t.Setenv("STRATA_META_BACKEND", "memory")
	t.Setenv("STRATA_DATA_BACKEND", "memory")

	var stdout, stderr bytes.Buffer
	a := newApp(&stdout, &stderr, []string{"bench-lifecycle", "--objects=10", "--replicas=0"})
	if err := a.run(context.Background()); err == nil {
		t.Fatalf("expected error for replicas=0")
	}
}
