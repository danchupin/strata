//go:build integration

package cassandra_test

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gocql/gocql"
	tccassandra "github.com/testcontainers/testcontainers-go/modules/cassandra"

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/meta/cassandra"
	"github.com/danchupin/strata/internal/meta/storetest"
)

// TestCassandraStoreContract runs the shared meta.Store contract against a
// Cassandra instance spun up via testcontainers. The container boots once per
// test function; each subtest gets its own fresh keyspace for isolation.
//
// Runs only under `go test -tags integration`. Skipped when STRATA_SCYLLA_TEST=1
// so the ScyllaDB CI workflow runs the Scylla suite alone.
func TestCassandraStoreContract(t *testing.T) {
	if os.Getenv("STRATA_SCYLLA_TEST") == "1" {
		t.Skip("STRATA_SCYLLA_TEST=1: ScyllaDB suite runs in TestScyllaStoreContract")
	}

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

	// Cluster-registry CRUD lives outside the default Run cases because
	// US-001 stubbed the cassandra/tikv impls; US-002 wires the real CAS
	// path here so the CassandraIntegration job exercises it without
	// flipping it on for every backend simultaneously.
	t.Run("ClusterRegistry", func(t *testing.T) {
		storetest.CaseClusterRegistry(t, newStore(t))
	})
}

// TestCassandraNullSentinelOnDisk regression-locks the cassandra timeuuid
// null-sentinel translation surfaced as a P1 latent bug in the prior cycle
// (ralph/s3-compat-90, fixed on main as `cassandraNullSentinel`). The
// canonical wire form `meta.NullVersionID = 00000000-0000-0000-0000-000000000000`
// is a v0 UUID, which Cassandra's server-side validation rejects on INSERT
// into the `objects.version_id timeuuid` column. The Store translates to/from
// the v1 sentinel `00000000-0000-1000-8000-000000000000` at every gocql
// boundary via versionToCQL / versionFromCQL.
//
// This test exercises the round-trip end-to-end against a real Cassandra
// container: PUT into a Suspended bucket → GET back → assert the in-memory
// VersionID is the canonical zero-UUID, AND assert the on-disk timeuuid
// (read via raw SELECT) is the v1 sentinel. A regression in either direction
// — INSERT failure (cassandra rejects v0) or wire-shape leak (v1 sentinel
// surfaces to S3 clients) — fails this test.
//
// Pairs with the storetest contract case `caseVersioningNullSentinel` which
// exercises the surface-level shape against all three backends.
func TestCassandraNullSentinelOnDisk(t *testing.T) {
	if os.Getenv("STRATA_SCYLLA_TEST") == "1" {
		t.Skip("STRATA_SCYLLA_TEST=1: ScyllaDB suite runs in TestScyllaStoreContract")
	}

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

	store, err := cassandra.Open(cassandra.SessionConfig{
		Hosts:       []string{host},
		Keyspace:    "strata_nullsentinel",
		LocalDC:     "datacenter1",
		Replication: "{'class': 'SimpleStrategy', 'replication_factor': '1'}",
		Timeout:     30 * time.Second,
	}, cassandra.Options{DefaultShardCount: 64})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const (
		bucketName       = "nullsentinel"
		key              = "doc"
		v1NullSentinelOnDisk = "00000000-0000-1000-8000-000000000000"
	)

	bucket, err := store.CreateBucket(ctx, bucketName, "owner", "STANDARD")
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	if err := store.SetBucketVersioning(ctx, bucketName, meta.VersioningSuspended); err != nil {
		t.Fatalf("set suspended: %v", err)
	}

	obj := &meta.Object{
		BucketID:     bucket.ID,
		Key:          key,
		StorageClass: "STANDARD",
		ETag:         "first",
		Size:         5,
		IsNull:       true,
		Mtime:        time.Now().UTC(),
		Manifest:     &data.Manifest{Class: "STANDARD", Size: 5},
	}
	if err := store.PutObject(ctx, obj, true); err != nil {
		t.Fatalf("put suspended-null: %v", err)
	}
	if obj.VersionID != meta.NullVersionID {
		t.Fatalf("in-memory VersionID after PUT: got %q want %q (canonical zero UUID)",
			obj.VersionID, meta.NullVersionID)
	}

	got, err := store.GetObject(ctx, bucket.ID, key, meta.NullVersionID)
	if err != nil {
		t.Fatalf("get by sentinel: %v", err)
	}
	if got.VersionID != meta.NullVersionID || !got.IsNull || got.ETag != "first" {
		t.Fatalf("round-trip object: %+v want VersionID=%q IsNull=true ETag=first",
			got, meta.NullVersionID)
	}
	gotByLiteral, err := store.GetObject(ctx, bucket.ID, key, meta.NullVersionLiteral)
	if err != nil {
		t.Fatalf("get by 'null' literal: %v", err)
	}
	if gotByLiteral.VersionID != meta.NullVersionID {
		t.Fatalf("literal-resolution VersionID: got %q want %q",
			gotByLiteral.VersionID, meta.NullVersionID)
	}

	// Raw SELECT proves the on-disk timeuuid is the v1 sentinel — NOT the
	// canonical v0 wire form. ALLOW FILTERING is fine for this single-row
	// fixture; production code never reads this way.
	var bucketIDGocql gocql.UUID
	copy(bucketIDGocql[:], bucket.ID[:])
	var rawVersionID gocql.UUID
	if err := store.Session().Query(
		`SELECT version_id FROM objects WHERE bucket_id=? AND key=? ALLOW FILTERING`,
		bucketIDGocql, key,
	).WithContext(ctx).Scan(&rawVersionID); err != nil {
		t.Fatalf("raw select version_id: %v", err)
	}
	if rawVersionID.String() != v1NullSentinelOnDisk {
		t.Fatalf("on-disk version_id: got %q want %q (cassandra timeuuid sentinel)",
			rawVersionID.String(), v1NullSentinelOnDisk)
	}
	if rawVersionID.String() == meta.NullVersionID {
		t.Fatalf("on-disk version_id leaked the canonical v0 sentinel %q — versionToCQL bypassed",
			meta.NullVersionID)
	}
}
