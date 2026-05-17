package meta

import (
	"testing"

	"github.com/google/uuid"
)

// TestValidatePlacementMode covers the four-way truth table for the
// PlacementMode validator: empty (legacy default), explicit weighted,
// explicit strict, and anything else (rejected with
// ErrInvalidPlacementMode). US-001 effective-placement.
func TestValidatePlacementMode(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want error
	}{
		{"empty-default", "", nil},
		{"weighted", PlacementModeWeighted, nil},
		{"strict", PlacementModeStrict, nil},
		{"uppercase-weighted", "WEIGHTED", ErrInvalidPlacementMode},
		{"uppercase-strict", "STRICT", ErrInvalidPlacementMode},
		{"unknown", "loose", ErrInvalidPlacementMode},
		{"surrounding-space", " strict", ErrInvalidPlacementMode},
		{"trailing-space", "strict ", ErrInvalidPlacementMode},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ValidatePlacementMode(tc.in); got != tc.want {
				t.Errorf("ValidatePlacementMode(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestNormalizePlacementMode confirms the empty-string default coerces
// to "weighted" while explicit values pass through verbatim. Downstream
// EffectivePolicy + UI renderers branch on the canonical form.
func TestNormalizePlacementMode(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", PlacementModeWeighted},
		{PlacementModeWeighted, PlacementModeWeighted},
		{PlacementModeStrict, PlacementModeStrict},
		// Unknown strings pass through — the function is not a validator.
		// Callers that need rejection use ValidatePlacementMode.
		{"loose", "loose"},
	}
	for _, tc := range cases {
		if got := NormalizePlacementMode(tc.in); got != tc.want {
			t.Errorf("NormalizePlacementMode(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestValidateShard covers the off-by-one + zero-/negative-input guard
// shared by every backend's ListBucketsShard. US-001 rebalance-scale-
// phase-2.
func TestValidateShard(t *testing.T) {
	cases := []struct {
		name        string
		shardID     int
		totalShards int
		wantErr     bool
	}{
		{"single-shard", 0, 1, false},
		{"first-of-three", 0, 3, false},
		{"last-of-three", 2, 3, false},
		{"off-by-one", 3, 3, true},
		{"negative-shard", -1, 3, true},
		{"zero-total", 0, 0, true},
		{"negative-total", 0, -1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateShard(tc.shardID, tc.totalShards)
			got := err != nil
			if got != tc.wantErr {
				t.Errorf("ValidateShard(%d,%d) err=%v wantErr=%v", tc.shardID, tc.totalShards, err, tc.wantErr)
			}
			if tc.wantErr && err != ErrInvalidShard {
				t.Errorf("ValidateShard(%d,%d) err=%v want=ErrInvalidShard", tc.shardID, tc.totalShards, err)
			}
		})
	}
}

// TestBucketShardIDDistribution feeds 1000 random UUIDs through
// BucketShardID with 3 and 10 shards and confirms the FNV-1a hash spreads
// uniformly within the AC's tolerance (3 → ~333±5%, 10 → ~100±10%).
// US-001 rebalance-scale-phase-2.
func TestBucketShardIDDistribution(t *testing.T) {
	const n = 1000
	ids := make([]uuid.UUID, n)
	for i := range ids {
		ids[i] = uuid.New()
	}

	check := func(t *testing.T, totalShards int, tolPct int) {
		t.Helper()
		buckets := make([]int, totalShards)
		for _, id := range ids {
			sid := BucketShardID(id, totalShards)
			if sid < 0 || sid >= totalShards {
				t.Fatalf("BucketShardID returned %d for totalShards=%d", sid, totalShards)
			}
			buckets[sid]++
		}
		expect := n / totalShards
		tol := n * tolPct / 100
		for sid, sz := range buckets {
			if sz < expect-tol || sz > expect+tol {
				t.Errorf("shard %d size=%d outside [%d,%d] for totalShards=%d", sid, sz, expect-tol, expect+tol, totalShards)
			}
		}
	}

	t.Run("3-shards-5pct", func(t *testing.T) { check(t, 3, 5) })
	t.Run("10-shards-10pct", func(t *testing.T) { check(t, 10, 10) })
}

// TestBucketShardIDStable confirms BucketShardID returns the same value
// for the same UUID across calls — required so a worker fan-out's shard
// assignment does not flap between restarts.
func TestBucketShardIDStable(t *testing.T) {
	id := uuid.MustParse("12345678-1234-1234-1234-123456789abc")
	first := BucketShardID(id, 7)
	for i := 0; i < 100; i++ {
		if got := BucketShardID(id, 7); got != first {
			t.Fatalf("BucketShardID flipped: first=%d, iter=%d got=%d", first, i, got)
		}
	}
}
