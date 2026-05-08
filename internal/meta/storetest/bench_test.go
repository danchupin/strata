package storetest_test

import (
	"testing"

	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/meta/memory"
	"github.com/danchupin/strata/internal/meta/storetest"
)

// BenchmarkMemoryStore is the in-tree baseline for the meta.Store hot-path
// harness. Cassandra and TiKV ship their own bench files under the
// integration build tag (see docs/site/content/architecture/benchmarks/meta-backend-comparison.md for
// the full rig). The memory backend is the reference: zero IO, lock-only
// contention, so per-op numbers floor what a network-backed backend can
// achieve.
//
// Run:
//
//	go test -bench=. -benchtime=5s ./internal/meta/storetest/...
//	go test -bench=. -benchtime=5m -short ./internal/meta/storetest/...  // smoke
//
// `-short` shrinks ListSize/AuditPreload from 100k/10k to 1k each so the
// suite finishes in seconds; drop it for the published numbers.
func BenchmarkMemoryStore(b *testing.B) {
	storetest.Bench(b, func() meta.Store { return memory.New() }, storetest.BenchDefaults)
}
