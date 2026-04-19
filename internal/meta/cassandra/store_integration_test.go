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

// TestCassandraStoreContract runs the shared meta.Store contract against a
// Cassandra instance spun up via testcontainers. The container boots once per
// test function; each subtest gets its own fresh keyspace for isolation.
//
// Runs only under `go test -tags integration`.
func TestCassandraStoreContract(t *testing.T) {
	ctx := context.Background()

	container, err := tccassandra.Run(ctx, "cassandra:5.0")
	if err != nil {
		t.Fatalf("start cassandra: %v", err)
	}
	t.Cleanup(func() {
		if err := container.Terminate(context.Background()); err != nil {
			t.Logf("terminate: %v", err)
		}
	})

	host, err := container.ConnectionHost(ctx)
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
