package tikv

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/meta"
)

func TestGCDualWriteFromEnv(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{"", true},
		{"on", true},
		{"true", true},
		{"yes", true},
		{"1", true},
		{"  ON  ", true},
		{"off", false},
		{"OFF", false},
		{"false", false},
		{"0", false},
		{"no", false},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			t.Setenv(EnvGCDualWrite, tc.raw)
			if got := GCDualWriteFromEnv(); got != tc.want {
				t.Errorf("raw=%q got=%v want=%v", tc.raw, got, tc.want)
			}
		})
	}
}

func TestGCOwnedLogicalShards(t *testing.T) {
	cases := []struct {
		shardID    int
		shardCount int
		wantLen    int
	}{
		{0, 1, meta.GCShardCount},
		{0, 2, meta.GCShardCount / 2},
		{1, 2, meta.GCShardCount / 2},
		{0, 4, meta.GCShardCount / 4},
		{3, 4, meta.GCShardCount / 4},
		{0, meta.GCShardCount, 1},
		{meta.GCShardCount - 1, meta.GCShardCount, 1},
	}
	for _, tc := range cases {
		got := gcOwnedLogicalShards(tc.shardID, tc.shardCount)
		if len(got) != tc.wantLen {
			t.Errorf("shardID=%d shardCount=%d len=%d want=%d", tc.shardID, tc.shardCount, len(got), tc.wantLen)
		}
		for _, s := range got {
			if s%tc.shardCount != tc.shardID {
				t.Errorf("shardID=%d shardCount=%d got logical %d (mod=%d)", tc.shardID, tc.shardCount, s, s%tc.shardCount)
			}
			if s < 0 || s >= meta.GCShardCount {
				t.Errorf("logical %d out of range [0, %d)", s, meta.GCShardCount)
			}
		}
	}

	const shardCount = 4
	seen := make(map[int]int)
	for sid := range shardCount {
		for _, s := range gcOwnedLogicalShards(sid, shardCount) {
			if prev, ok := seen[s]; ok {
				t.Errorf("logical %d in shard %d and shard %d", s, prev, sid)
			}
			seen[s] = sid
		}
	}
	if len(seen) != meta.GCShardCount {
		t.Errorf("union size=%d want %d", len(seen), meta.GCShardCount)
	}
}

// TestGCQueueKeyV2Layout pins the v2 wire shape: prefix, escaped region,
// 2-byte BE shard, 8-byte BE timestamp, escaped oid. Per-shard prefix is a
// strict prefix of every key with that shard.
func TestGCQueueKeyV2Layout(t *testing.T) {
	const region = "us-east-1"
	const oid = "chunk:abc"
	const ts = uint64(0x0102030405060708)
	const shardID = uint16(7)

	key := GCQueueKeyV2(region, shardID, ts, oid)
	prefix := GCQueueShardPrefixV2(region, shardID)

	if !bytes.HasPrefix(key, prefix) {
		t.Fatalf("key %x missing shard prefix %x", key, prefix)
	}
	regionPrefix := GCQueueRegionPrefixV2(region)
	if !bytes.HasPrefix(key, regionPrefix) {
		t.Fatalf("key %x missing region prefix %x", key, regionPrefix)
	}
	// Shard prefix must extend region prefix by exactly 2 bytes (the BE shardID).
	if len(prefix)-len(regionPrefix) != 2 {
		t.Fatalf("shard prefix - region prefix = %d, want 2", len(prefix)-len(regionPrefix))
	}
	if want := byte(0); prefix[len(prefix)-2] != want {
		t.Fatalf("shard hi byte = %x want %x", prefix[len(prefix)-2], want)
	}
	if want := byte(7); prefix[len(prefix)-1] != want {
		t.Fatalf("shard lo byte = %x want %x", prefix[len(prefix)-1], want)
	}

	// Different shards yield disjoint prefixes (forward range scan over one
	// shard prefix never bleeds into siblings).
	other := GCQueueShardPrefixV2(region, shardID+1)
	if bytes.HasPrefix(key, other) {
		t.Fatalf("key %x leaked into sibling prefix %x", key, other)
	}

	// v2 prefix must NOT overlap legacy prefix — a legacy region scan must
	// not pull v2 keys and vice-versa.
	legacy := GCQueuePrefix(region)
	if bytes.HasPrefix(key, legacy) {
		t.Fatalf("v2 key %x overlaps legacy prefix %x", key, legacy)
	}
}

// TestGCQueueDualWriteToggle confirms the dual-write knob gates fan-out at
// the wire layer: when on, a list scan of the legacy prefix returns rows;
// when off, only the v2 prefix has them. Reads always prefer v2.
func TestGCQueueDualWriteToggle(t *testing.T) {
	ctx := context.Background()
	chunks := []data.ChunkRef{
		{Cluster: "c1", Pool: "p1", OID: "obj-1", Size: 11},
		{Cluster: "c2", Pool: "p2", OID: "obj-2", Size: 22},
	}

	t.Run("dual_write_on_writes_both_sides", func(t *testing.T) {
		s := openWithBackend(newMemBackend())
		s.gcDualWrite = true
		t.Cleanup(func() { _ = s.Close() })

		if err := s.EnqueueChunkDeletion(ctx, "r", chunks); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		legacyKeys := scanPrefix(t, s, GCQueuePrefix("r"))
		v2Keys := scanPrefix(t, s, GCQueueRegionPrefixV2("r"))
		if len(legacyKeys) != len(chunks) {
			t.Fatalf("legacy got=%d want=%d", len(legacyKeys), len(chunks))
		}
		if len(v2Keys) != len(chunks) {
			t.Fatalf("v2 got=%d want=%d", len(v2Keys), len(chunks))
		}

		entries, err := s.ListGCEntries(ctx, "r", time.Now().Add(time.Hour), 100)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		// Dedup: each oid appears exactly once even though both sides have it.
		oids := map[string]int{}
		for _, e := range entries {
			oids[e.Chunk.OID]++
		}
		for _, c := range chunks {
			if oids[c.OID] != 1 {
				t.Fatalf("oid %q seen %d times after dedup", c.OID, oids[c.OID])
			}
		}

		// Ack drops both sides.
		for _, e := range entries {
			if err := s.AckGCEntry(ctx, "r", e); err != nil {
				t.Fatalf("ack %s: %v", e.Chunk.OID, err)
			}
		}
		if k := scanPrefix(t, s, GCQueuePrefix("r")); len(k) != 0 {
			t.Fatalf("legacy after ack: %d", len(k))
		}
		if k := scanPrefix(t, s, GCQueueRegionPrefixV2("r")); len(k) != 0 {
			t.Fatalf("v2 after ack: %d", len(k))
		}
	})

	t.Run("dual_write_off_writes_v2_only", func(t *testing.T) {
		s := openWithBackend(newMemBackend())
		s.gcDualWrite = false
		t.Cleanup(func() { _ = s.Close() })

		if err := s.EnqueueChunkDeletion(ctx, "r", chunks); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		if k := scanPrefix(t, s, GCQueuePrefix("r")); len(k) != 0 {
			t.Fatalf("legacy keys leaked while dual-write off: %d", len(k))
		}
		v2Keys := scanPrefix(t, s, GCQueueRegionPrefixV2("r"))
		if len(v2Keys) != len(chunks) {
			t.Fatalf("v2 got=%d want=%d", len(v2Keys), len(chunks))
		}
	})
}

// TestGCQueueShardFanOutNativePrefix exercises ListGCEntriesShard against
// the v2 per-shard prefix scan path: every entry comes back exactly once
// across the disjoint set of runtime shards, and the legacy fallback is
// not consulted (dual-write off forces the v2 path to be sufficient).
func TestGCQueueShardFanOutNativePrefix(t *testing.T) {
	ctx := context.Background()
	s := openWithBackend(newMemBackend())
	s.gcDualWrite = false
	t.Cleanup(func() { _ = s.Close() })

	const total = 200
	chunks := make([]data.ChunkRef, total)
	for i := range chunks {
		chunks[i] = data.ChunkRef{Cluster: "c", Pool: "p", OID: makeOID(i), Size: int64(i)}
	}
	if err := s.EnqueueChunkDeletion(ctx, "r", chunks); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	const shardCount = 4
	seen := map[string]int{}
	for sid := range shardCount {
		out, err := s.ListGCEntriesShard(ctx, "r", sid, shardCount, time.Now().Add(time.Hour), 1000)
		if err != nil {
			t.Fatalf("shard %d list: %v", sid, err)
		}
		for _, e := range out {
			if prev, ok := seen[e.Chunk.OID]; ok {
				t.Fatalf("oid %s in shard %d and %d", e.Chunk.OID, prev, sid)
			}
			seen[e.Chunk.OID] = sid
			if got, want := meta.GCShardID(e.Chunk.OID)%shardCount, sid; got != want {
				t.Fatalf("oid %s landed in shard %d, expected %d", e.Chunk.OID, sid, got)
			}
		}
	}
	if len(seen) != total {
		t.Fatalf("union=%d want %d", len(seen), total)
	}
}

// TestGCQueueLegacyFallback rebuilds an "operator forgot to drain" state:
// only legacy rows exist (simulated by toggling dual-write at runtime).
// The reader must still surface them while dual-write is on, then stop
// once flipped off.
func TestGCQueueLegacyFallback(t *testing.T) {
	ctx := context.Background()
	s := openWithBackend(newMemBackend())
	s.gcDualWrite = true
	t.Cleanup(func() { _ = s.Close() })

	chunks := []data.ChunkRef{
		{Cluster: "c", Pool: "p", OID: "legacy-only-1"},
		{Cluster: "c", Pool: "p", OID: "legacy-only-2"},
	}
	if err := s.EnqueueChunkDeletion(ctx, "r", chunks); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	// Wipe v2 by direct scan + delete to simulate "legacy-only" state.
	wipePrefix(t, s, GCQueueRegionPrefixV2("r"))

	got, err := s.ListGCEntries(ctx, "r", time.Now().Add(time.Hour), 100)
	if err != nil {
		t.Fatalf("list with fallback: %v", err)
	}
	if len(got) != len(chunks) {
		t.Fatalf("fallback got=%d want=%d", len(got), len(chunks))
	}

	// Flip dual-write off → legacy fallback stops.
	s.gcDualWrite = false
	got, err = s.ListGCEntries(ctx, "r", time.Now().Add(time.Hour), 100)
	if err != nil {
		t.Fatalf("list after flip: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("after flip got=%d want=0", len(got))
	}
}

// makeOID returns a stable oid that hashes into a known logical shard for
// the seed `i`. The exact shard does not matter — disjoint coverage is
// what the test asserts.
func makeOID(i int) string {
	return "obj-" + itoa(i)
}

func itoa(i int) string {
	const digits = "0123456789"
	if i == 0 {
		return "0"
	}
	out := make([]byte, 0, 10)
	for i > 0 {
		out = append([]byte{digits[i%10]}, out...)
		i /= 10
	}
	return string(out)
}

func scanPrefix(t *testing.T, s *Store, prefix []byte) [][]byte {
	t.Helper()
	ctx := context.Background()
	txn, err := s.kv.Begin(ctx, false)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer txn.Rollback()
	pairs, err := txn.Scan(ctx, prefix, prefixEnd(prefix), 0)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	out := make([][]byte, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, append([]byte(nil), p.Key...))
	}
	return out
}

func wipePrefix(t *testing.T, s *Store, prefix []byte) {
	t.Helper()
	ctx := context.Background()
	keys := scanPrefix(t, s, prefix)
	if len(keys) == 0 {
		return
	}
	txn, err := s.kv.Begin(ctx, false)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for _, k := range keys {
		if err := txn.Delete(k); err != nil {
			_ = txn.Rollback()
			t.Fatalf("delete: %v", err)
		}
	}
	if err := txn.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}
