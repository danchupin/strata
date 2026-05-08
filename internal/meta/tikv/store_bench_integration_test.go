//go:build integration

package tikv

import (
	"context"
	"testing"
	"time"

	"github.com/tikv/client-go/v2/txnkv"

	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/meta/storetest"
	"github.com/danchupin/strata/internal/meta/tikv/tikvtest"
)

// BenchmarkTiKVStore runs the meta.Store hot-path harness from
// internal/meta/storetest against a real PD + TiKV pair. Cluster sourcing
// matches TestTiKVStoreContract: STRATA_TIKV_TEST_PD_ENDPOINTS env wins,
// otherwise tikvtest.AcquireCluster brings up the testcontainers stack.
//
// Run:
//
//	go test -tags integration -bench=BenchmarkTiKVStore -benchtime=5m \
//	    ./internal/meta/tikv/...
//
// See docs/site/content/architecture/benchmarks/meta-backend-comparison.md for the published rig and
// how to interpret the numbers vs Cassandra.
func BenchmarkTiKVStore(b *testing.B) {
	ctx := context.Background()

	endpoints, cleanup := tikvtest.AcquireCluster(ctx, b)
	b.Cleanup(cleanup)

	cli, err := txnkv.NewClient(endpoints)
	if err != nil {
		b.Fatalf("dial PD %v: %v", endpoints, err)
	}
	b.Cleanup(func() { _ = cli.Close() })

	probeCtx, probeCancel := context.WithTimeout(ctx, 30*time.Second)
	defer probeCancel()
	be := &tikvBackend{cli: cli}
	if err := tikvtest.WaitProbe(probeCtx, be); err != nil {
		b.Fatalf("probe TiKV cluster at %v: %v", endpoints, err)
	}

	storetest.Bench(b, func() meta.Store {
		return openWithBackend(&tikvBackend{cli: cli})
	}, storetest.BenchDefaults)
}
