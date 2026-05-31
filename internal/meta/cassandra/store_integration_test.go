//go:build integration

package cassandra_test

import (
	"context"
	"fmt"
	"hash/fnv"
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
			Timeout:     60 * time.Second,
		}, cassandra.Options{DefaultShardCount: 64})
		if err != nil {
			t.Fatalf("open keyspace %s: %v", ks, err)
		}
		t.Cleanup(func() { _ = store.Close() })
		return store
	}

	storetest.Run(t, newStore)
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
		Timeout:     60 * time.Second,
	}, cassandra.Options{DefaultShardCount: 64})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const (
		bucketName           = "nullsentinel"
		key                  = "doc"
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

// TestCassandraPerBucketShardResolution is the discriminating red/green proof
// for US-001: the Cassandra hot path must address objects by the bucket's
// stored shard_count, NOT the running process's STRATA_BUCKET_SHARDS default.
//
// The bug only manifests when the writer and the reader disagree on the shard
// count — exactly what happens when a bucket created under one STRATA_BUCKET_SHARDS
// is later served by a process started with a different default. We reproduce it
// by opening two stores over the SAME keyspace with different DefaultShardCount:
// the writer store creates the bucket (stamping shard_count) and writes objects
// into the bucket's true layout; the reader store, running a different default,
// must resolve the bucket's stored count to find them.
//
// Against the pre-US-001 code the reader uses s.defaultShard and point-reads the
// wrong partition → every GET/DELETE whose fnv(key)%writerCount differs from
// fnv(key)%readerCount returns ErrObjectNotFound (RED). After US-001 the reader
// resolves the bucket's shard_count and the full lifecycle round-trips (GREEN).
// Covers both a higher (128) and a lower (16) non-default count per the AC.
func TestCassandraPerBucketShardResolution(t *testing.T) {
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

	openStore := func(keyspace string, shards int) *cassandra.Store {
		store, err := cassandra.Open(cassandra.SessionConfig{
			Hosts:       []string{host},
			Keyspace:    keyspace,
			LocalDC:     "datacenter1",
			Replication: "{'class': 'SimpleStrategy', 'replication_factor': '1'}",
			Timeout:     60 * time.Second,
		}, cassandra.Options{DefaultShardCount: shards})
		if err != nil {
			t.Fatalf("open keyspace %s (shards=%d): %v", keyspace, shards, err)
		}
		t.Cleanup(func() { _ = store.Close() })
		return store
	}

	// The reader always runs the process default of 64. Each leg creates its
	// bucket under a writer whose default differs (128 above, 16 below) so the
	// reader is forced to honour the stored count.
	cases := []struct {
		name        string
		keyspace    string
		bucket      string
		writerCount int
	}{
		{"higher_than_default", "strata_shardres_hi", "bkt-hi", 128},
		{"lower_than_default", "strata_shardres_lo", "bkt-lo", 16},
	}

	const readerCount = 64
	const nKeys = 60

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			writer := openStore(tc.keyspace, tc.writerCount)
			reader := openStore(tc.keyspace, readerCount)

			bucket, err := writer.CreateBucket(ctx, tc.bucket, "owner", "STANDARD")
			if err != nil {
				t.Fatalf("create bucket: %v", err)
			}
			if bucket.ShardCount != tc.writerCount {
				t.Fatalf("created bucket shard_count: got %d want %d", bucket.ShardCount, tc.writerCount)
			}

			keys := make([]string, nKeys)
			for i := 0; i < nKeys; i++ {
				keys[i] = fmt.Sprintf("obj-%03d", i)
				o := &meta.Object{
					BucketID:     bucket.ID,
					Key:          keys[i],
					StorageClass: "STANDARD",
					ETag:         fmt.Sprintf("etag-%03d", i),
					Size:         int64(i + 1),
					Mtime:        time.Now().UTC(),
					Manifest:     &data.Manifest{Class: "STANDARD", Size: int64(i + 1)},
				}
				if err := writer.PutObject(ctx, o, false); err != nil {
					t.Fatalf("writer put %s: %v", keys[i], err)
				}
			}

			// Sanity: at least one key must address a different partition under
			// the reader's default than under the writer's count, otherwise the
			// test could pass vacuously even with the bug.
			differing := 0
			for _, k := range keys {
				if shardOfKey(k, tc.writerCount) != shardOfKey(k, readerCount) {
					differing++
				}
			}
			if differing == 0 {
				t.Fatalf("no key partitions differ between writer(%d) and reader(%d) — test is vacuous", tc.writerCount, readerCount)
			}
			t.Logf("%d/%d keys address a different partition under reader default %d vs stored %d",
				differing, nKeys, readerCount, tc.writerCount)

			// Reader point-GET each key: pre-US-001 the reader addresses the
			// wrong partition for the `differing` keys and 404s.
			for i, k := range keys {
				got, err := reader.GetObject(ctx, bucket.ID, k, "")
				if err != nil {
					t.Fatalf("reader get %s: %v (per-bucket shard resolution regressed — reader used its own default %d instead of stored %d)",
						k, err, readerCount, tc.writerCount)
				}
				if got.ETag != fmt.Sprintf("etag-%03d", i) {
					t.Fatalf("reader get %s: etag=%q want etag-%03d", k, got.ETag, i)
				}
			}

			// Reader LIST must surface the full key set under the resolved count.
			res, err := reader.ListObjects(ctx, bucket.ID, meta.ListOptions{Limit: 5000})
			if err != nil {
				t.Fatalf("reader list: %v", err)
			}
			if len(res.Objects) != nKeys {
				t.Fatalf("reader list: got %d want %d", len(res.Objects), nKeys)
			}

			// Reader DELETE addresses the same partition the writer wrote; the
			// row is gone afterwards.
			victim := keys[nKeys-1]
			if _, err := reader.DeleteObject(ctx, bucket.ID, victim, "", false); err != nil {
				t.Fatalf("reader delete %s: %v", victim, err)
			}
			if _, err := reader.GetObject(ctx, bucket.ID, victim, ""); err != meta.ErrObjectNotFound {
				t.Fatalf("reader get after delete %s: got %v want ErrObjectNotFound", victim, err)
			}
		})
	}
}

// shardOfKey mirrors the package-internal shardOf used by the store, so the
// test can assert writer/reader partition divergence without exporting it.
func shardOfKey(key string, n int) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return int(h.Sum32() % uint32(n))
}
