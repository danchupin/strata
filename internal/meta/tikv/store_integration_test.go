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

// TestTiKVStoreContract runs the shared meta.Store contract suite against a
// real PD + TiKV pair. Cluster bring-up + probe live in
// internal/meta/tikv/tikvtest so the race harness's TiKV variant
// (TestRaceMixedOpsTiKV in internal/s3api) can share the same shape.
//
// Cluster sourcing (in priority order):
//
//  1. STRATA_TIKV_TEST_PD_ENDPOINTS — comma-separated PD client addresses.
//     Operator-provided cluster; tests run unconditionally against it.
//     CI workflows (US-017) supply this so the suite exercises whatever
//     PD/TiKV the workflow already brought up via docker-compose.
//
//  2. Otherwise: spawn pingcap/pd + pingcap/tikv via testcontainers-go on a
//     private docker network with the host-gateway advertise pattern.
//     Suitable for local `make test-integration`.
//
// If both paths fail (no env, no Docker, image pull error), the test
// t.Skipf's with the underlying error so a sandboxed CI runner without
// container support still marks the suite passing — the contract surface is
// already validated against the in-process memory backend (parity oracle).
//
// The cluster is shared across subtests; contract cases use distinct bucket
// names per case so writes do not collide. A fresh `go test` invocation
// brings up a fresh cluster.
//
// Runs only under `go test -tags integration`.
func TestTiKVStoreContract(t *testing.T) {
	ctx := context.Background()

	endpoints, cleanup := tikvtest.AcquireCluster(ctx, t)
	t.Cleanup(cleanup)

	cli, err := txnkv.NewClient(endpoints)
	if err != nil {
		t.Fatalf("dial PD %v: %v", endpoints, err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	// Probe with retry — the cluster may need a few seconds after container
	// readiness to elect a region leader and accept timestamps. tikvBackend
	// is package-private, so the contract test probes via the local backend
	// adapter directly rather than constructing a separate tikv.Store.
	probeCtx, probeCancel := context.WithTimeout(ctx, 30*time.Second)
	defer probeCancel()
	be := &tikvBackend{cli: cli}
	if err := tikvtest.WaitProbe(probeCtx, be); err != nil {
		t.Fatalf("probe TiKV cluster at %v: %v", endpoints, err)
	}

	storetest.Run(t, func(t *testing.T) meta.Store {
		// Each subtest gets a Store on the shared backend handle. The
		// contract cases use unique bucket / object names so cross-case
		// state leakage is bounded.
		return openWithBackend(&tikvBackend{cli: cli})
	})
}
