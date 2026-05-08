package tikv

import (
	"os"
	"strings"

	"github.com/danchupin/strata/internal/meta"
)

// EnvGCDualWrite controls whether EnqueueChunkDeletion fan-outs to both the
// legacy `s/qg/<region>...` prefix and the Phase 2 sharded `s/qG/...`
// prefix during the US-003 cutover. Default `on` until the operator has
// confirmed the legacy prefix has fully drained. Mirrors the cassandra
// backend's same-named knob (internal/meta/cassandra/gc.go).
const EnvGCDualWrite = "STRATA_GC_DUAL_WRITE"

// GCDualWriteFromEnv parses STRATA_GC_DUAL_WRITE into a bool. Empty / unknown
// → true (dual-write on). Recognised "off" forms: "off", "false", "0", "no".
func GCDualWriteFromEnv() bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv(EnvGCDualWrite)))
	switch v {
	case "off", "false", "0", "no":
		return false
	default:
		return true
	}
}

// gcOwnedLogicalShards returns the subset of [0, meta.GCShardCount) that
// the runtime shard `shardID` (of `shardCount`) owns under modulo mapping.
// shardCount=1 returns every logical shard; shardCount=meta.GCShardCount
// returns exactly one. Mirrors the cassandra backend's helper.
func gcOwnedLogicalShards(shardID, shardCount int) []int {
	out := make([]int, 0, meta.GCShardCount/shardCount+1)
	for s := shardID; s < meta.GCShardCount; s += shardCount {
		out = append(out, s)
	}
	return out
}
