//go:build integration

package cassandra_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	tccassandra "github.com/testcontainers/testcontainers-go/modules/cassandra"

	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/meta/cassandra"
	"github.com/danchupin/strata/internal/meta/storetest"
)

// BenchmarkCassandraStore runs the meta.Store hot-path harness from
// internal/meta/storetest against a Cassandra testcontainer. Shape mirrors
// TestCassandraStoreContract; see docs/benchmarks/meta-backend-comparison.md
// for the rig + how to compare numbers against the TiKV variant.
//
// Run:
//
//	go test -tags integration -bench=BenchmarkCassandraStore -benchtime=5m \
//	    ./internal/meta/cassandra/...
//
// The container boots once for the entire benchmark run; each sub-bench
// gets a fresh keyspace so per-bench fixtures do not collide.
func BenchmarkCassandraStore(b *testing.B) {
	ctx := context.Background()

	container, err := tccassandra.Run(ctx, "cassandra:5.0")
	if err != nil {
		b.Fatalf("start cassandra: %v", err)
	}
	b.Cleanup(func() {
		if err := container.Terminate(context.Background()); err != nil {
			b.Logf("terminate: %v", err)
		}
	})

	host, err := container.ConnectionHost(ctx)
	if err != nil {
		b.Fatalf("connection host: %v", err)
	}

	var seq int64
	newStore := func() meta.Store {
		n := atomic.AddInt64(&seq, 1)
		ks := fmt.Sprintf("strata_bench_%d", n)
		store, err := cassandra.Open(cassandra.SessionConfig{
			Hosts:       []string{host},
			Keyspace:    ks,
			LocalDC:     "datacenter1",
			Replication: "{'class': 'SimpleStrategy', 'replication_factor': '1'}",
			Timeout:     30 * time.Second,
		}, cassandra.Options{DefaultShardCount: 64})
		if err != nil {
			b.Fatalf("open keyspace %s: %v", ks, err)
		}
		return store
	}

	storetest.Bench(b, newStore, storetest.BenchDefaults)
}
