//go:build integration

package cassandra_test

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/meta/cassandra"
	"github.com/danchupin/strata/internal/meta/storetest"
)

// TestScyllaStoreContract runs the shared meta.Store contract against a
// ScyllaDB container. ScyllaDB is wire-compatible with Cassandra and is
// supported as a drop-in metadata backend (see docs/backends/scylla.md).
//
// Gated by STRATA_SCYLLA_TEST=1 so the default `go test -tags integration`
// run keeps targeting Cassandra. CI workflow .github/workflows/ci-scylla.yml
// flips the env var to exercise the Scylla path on a weekly schedule + manual
// dispatch. Image override via STRATA_SCYLLA_IMAGE (default scylladb/scylla:5.4).
func TestScyllaStoreContract(t *testing.T) {
	if os.Getenv("STRATA_SCYLLA_TEST") != "1" {
		t.Skip("set STRATA_SCYLLA_TEST=1 to run the ScyllaDB contract suite")
	}

	image := os.Getenv("STRATA_SCYLLA_IMAGE")
	if image == "" {
		image = "scylladb/scylla:5.4"
	}

	ctx := context.Background()
	ctr, err := testcontainers.Run(ctx, image,
		testcontainers.WithExposedPorts("9042/tcp"),
		testcontainers.WithCmd(
			"--smp", "1",
			"--memory", "750M",
			"--overprovisioned", "1",
			"--reactor-backend=epoll",
			"--developer-mode=1",
		),
		testcontainers.WithWaitStrategy(
			wait.ForListeningPort("9042/tcp").WithStartupTimeout(5*time.Minute),
			wait.ForLog("Scylla version").WithStartupTimeout(5*time.Minute),
		),
	)
	if err != nil {
		t.Fatalf("start scylla: %v", err)
	}
	t.Cleanup(func() {
		if err := ctr.Terminate(context.Background()); err != nil {
			t.Logf("terminate: %v", err)
		}
	})

	host, err := ctr.PortEndpoint(ctx, "9042/tcp", "")
	if err != nil {
		t.Fatalf("connection host: %v", err)
	}

	var seq int64
	newStore := func(t *testing.T) meta.Store {
		n := atomic.AddInt64(&seq, 1)
		ks := fmt.Sprintf("strata_%d", n)
		store, err := cassandra.Open(cassandra.SessionConfig{
			Hosts:       []string{host},
			Keyspace:    ks,
			LocalDC:     "datacenter1",
			Replication: "{'class': 'SimpleStrategy', 'replication_factor': '1'}",
			Timeout:     30 * time.Second,
		}, cassandra.Options{DefaultShardCount: 64})
		if err != nil {
			t.Fatalf("open keyspace %s: %v", ks, err)
		}
		t.Cleanup(func() { _ = store.Close() })
		return store
	}

	storetest.Run(t, newStore)
}
