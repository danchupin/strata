package rados

import (
	"os"
	"strconv"
)

// BatchOpsFromEnv is the exported entry point for cephimpl/ (the real
// RADOS backend in its own Go module).
func BatchOpsFromEnv() bool { return batchOpsFromEnv() }

// batchOpsFromEnv reads STRATA_RADOS_BATCH_OPS. Default is false (off):
// the synthetic bench in scripts/bench-rados-ops.sh shows no measurable
// gain over the per-op default until xattrs are added to the PUT/GET hot
// path, which no caller does today. Operators experimenting with future
// xattr writers can flip this to true to wire the batched helpers in
// internal/data/rados/ops.go.
//
// See docs/site/content/architecture/benchmarks/rados-ops.md for the gate
// logic and bench numbers.
func batchOpsFromEnv() bool {
	if v := os.Getenv("STRATA_RADOS_BATCH_OPS"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return false
}
