package cassandra

import (
	"testing"

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

	// Disjoint + complete coverage when union is taken across all runtime
	// shards: every logical shard appears exactly once.
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
