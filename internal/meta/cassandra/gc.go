package cassandra

import (
	"os"
	"strings"
)

// EnvGCDualWrite controls whether EnqueueChunkDeletion fan-outs to both the
// legacy `gc_queue` partition and the new `gc_entries_v2` shard-partitioned
// table during the Phase 2 cutover (US-002). Default `on` until the operator
// has confirmed `gc_queue` has fully drained.
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
