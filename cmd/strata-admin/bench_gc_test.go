package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestApp_BenchGC_RunsAgainstMemory pins the smoke contract from US-003: the
// bench-gc subcommand exits 0 against in-memory backends, drains the seeded
// entries, and emits a single JSONL row whose fields match the benchResult
// shape downstream tooling consumes.
func TestApp_BenchGC_RunsAgainstMemory(t *testing.T) {
	t.Setenv("STRATA_META_BACKEND", "memory")
	t.Setenv("STRATA_DATA_BACKEND", "memory")

	var stdout, stderr bytes.Buffer
	a := newApp(&stdout, &stderr, []string{"bench-gc", "--entries=10", "--concurrency=1"})
	if err := a.run(context.Background()); err != nil {
		t.Fatalf("bench-gc: %v stderr=%s", err, stderr.String())
	}
	out := strings.TrimSpace(stdout.String())
	if out == "" {
		t.Fatalf("bench-gc emitted no JSON; stderr=%s", stderr.String())
	}
	var res benchResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("decode bench result: %v (raw=%q)", err, out)
	}
	if res.Bench != "gc" {
		t.Errorf("bench=%q want gc", res.Bench)
	}
	if res.Entries != 10 {
		t.Errorf("entries=%d want 10", res.Entries)
	}
	if res.Concurrency != 1 {
		t.Errorf("concurrency=%d want 1", res.Concurrency)
	}
	if res.MetaBackend != "memory" || res.DataBackend != "memory" {
		t.Errorf("backends=%s/%s want memory/memory", res.MetaBackend, res.DataBackend)
	}
	if res.ElapsedMs < 0 {
		t.Errorf("elapsed_ms=%d must be non-negative", res.ElapsedMs)
	}
}

// TestApp_BenchGC_RejectsBadConcurrency: the [1, 256] clamp is enforced at
// the flag boundary so operators get a clear error instead of a silently
// clamped value (mirrors the worker-side clampConcurrency helper).
func TestApp_BenchGC_RejectsBadConcurrency(t *testing.T) {
	t.Setenv("STRATA_META_BACKEND", "memory")
	t.Setenv("STRATA_DATA_BACKEND", "memory")

	var stdout, stderr bytes.Buffer
	a := newApp(&stdout, &stderr, []string{"bench-gc", "--entries=10", "--concurrency=0"})
	if err := a.run(context.Background()); err == nil {
		t.Fatalf("expected error for concurrency=0")
	}
}

// TestApp_BenchGC_MultiShard pins the US-006 multi-leader shape: --shards=N
// spawns N workers in parallel against a shared in-memory backend, drains
// the full seeded set, and stamps the shard count on the JSON output for
// downstream artifact correlation.
func TestApp_BenchGC_MultiShard(t *testing.T) {
	t.Setenv("STRATA_META_BACKEND", "memory")
	t.Setenv("STRATA_DATA_BACKEND", "memory")

	var stdout, stderr bytes.Buffer
	a := newApp(&stdout, &stderr, []string{"bench-gc", "--entries=200", "--concurrency=4", "--shards=3"})
	if err := a.run(context.Background()); err != nil {
		t.Fatalf("bench-gc: %v stderr=%s", err, stderr.String())
	}
	var res benchResult
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout.String())), &res); err != nil {
		t.Fatalf("decode: %v (raw=%q)", err, stdout.String())
	}
	if res.Shards != 3 {
		t.Errorf("shards=%d want 3", res.Shards)
	}
	if res.Entries != 200 {
		t.Errorf("entries=%d want 200", res.Entries)
	}
}

// TestApp_BenchGC_RejectsBadShards: same boundary contract as concurrency.
func TestApp_BenchGC_RejectsBadShards(t *testing.T) {
	t.Setenv("STRATA_META_BACKEND", "memory")
	t.Setenv("STRATA_DATA_BACKEND", "memory")

	var stdout, stderr bytes.Buffer
	a := newApp(&stdout, &stderr, []string{"bench-gc", "--entries=10", "--shards=0"})
	if err := a.run(context.Background()); err == nil {
		t.Fatalf("expected error for shards=0")
	}
}
