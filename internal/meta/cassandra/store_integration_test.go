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
	"github.com/danchupin/strata/internal/reshard"
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

// TestCassandraReshardTransitional is the discriminating red/green proof for the
// US-002 transitional read/write model on Cassandra (the storetest contract case
// is a no-op on the shard-agnostic memory/TiKV backends).
//
// The headline guarantee: a write that lands DURING a reshard must go to the
// TARGET layout so it survives the post-flip active-count rotation. We seed a
// bucket at shard_count=8, StartReshard to 16, write keys whose source shard
// (fnv%8) differs from their target shard (fnv%16), then CompleteReshard (which
// rotates active→16) and GET them.
//
//   - WITH US-002: the during-job writes landed in the target layout (fnv%16), so
//     the post-flip read addresses the right partition → GREEN.
//   - WITHOUT US-002: writes went to the source layout (fnv%8); after the flip the
//     read uses fnv%16 ≠ fnv%8 for the diverging keys → ErrObjectNotFound (RED).
//
// We also assert the in-flight union read: an overwrite during the job returns
// the new value and lists exactly once (target wins the (key, version_id)
// collision; the stale source row is never double-emitted). NOTE we do NOT
// assert post-flip survival of un-rewritten pre-flip rows — US-002 has no
// migration worker (US-003), so those rows remain physically in the source
// layout; the worker is what gates CompleteReshard behind cleanup-before-flip.
func TestCassandraReshardTransitional(t *testing.T) {
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

	const activeCount = 8
	const targetCount = 16

	store, err := cassandra.Open(cassandra.SessionConfig{
		Hosts:       []string{host},
		Keyspace:    "strata_reshard_trans",
		LocalDC:     "datacenter1",
		Replication: "{'class': 'SimpleStrategy', 'replication_factor': '1'}",
		Timeout:     60 * time.Second,
	}, cassandra.Options{DefaultShardCount: activeCount})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	bucket, err := store.CreateBucket(ctx, "rsh-trans", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	if bucket.ShardCount != activeCount {
		t.Fatalf("bucket shard_count: got %d want %d", bucket.ShardCount, activeCount)
	}

	put := func(key, etag string, size int64) {
		o := &meta.Object{
			BucketID: bucket.ID, Key: key,
			StorageClass: "STANDARD", ETag: etag, Size: size,
			Mtime:    time.Now().UTC(),
			Manifest: &data.Manifest{Class: "STANDARD", Size: size},
		}
		if err := store.PutObject(ctx, o, false); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}

	// Seed pre-flip keys (source layout). Collect the ones whose source and
	// target partitions diverge — those are the keys the bug strands post-flip.
	const nKeys = 120
	var diverging []string
	for i := 0; i < nKeys; i++ {
		key := fmt.Sprintf("obj-%03d", i)
		put(key, "pre", 1)
		if shardOfKey(key, activeCount) != shardOfKey(key, targetCount) {
			diverging = append(diverging, key)
		}
	}
	if len(diverging) < 4 {
		t.Fatalf("only %d/%d keys diverge between active=%d and target=%d — test would be near-vacuous",
			len(diverging), nKeys, activeCount, targetCount)
	}
	t.Logf("%d/%d keys address a different partition under target=%d vs active=%d",
		len(diverging), nKeys, targetCount, activeCount)

	// Flip into the in-flight state.
	if _, err := store.StartReshard(ctx, bucket.ID, targetCount); err != nil {
		t.Fatalf("start reshard: %v", err)
	}

	// During the job: overwrite a diverging pre-flip key. The new row lands in
	// the target layout; the stale source row must be cleared so the union read
	// neither double-emits nor returns the old value.
	overwritten := diverging[0]
	put(overwritten, "post", 2)

	if got, err := store.GetObject(ctx, bucket.ID, overwritten, ""); err != nil {
		t.Fatalf("in-flight get overwritten %s: %v", overwritten, err)
	} else if got.ETag != "post" || got.Size != 2 {
		t.Fatalf("in-flight get overwritten %s: got etag=%q size=%d want post/2", overwritten, got.ETag, got.Size)
	}

	// In-flight LIST: full set, every key exactly once (no gaps, no duplicates).
	seen := make(map[string]int)
	marker := ""
	for {
		res, err := store.ListObjects(ctx, bucket.ID, meta.ListOptions{Limit: 1000, Marker: marker})
		if err != nil {
			t.Fatalf("in-flight list (marker=%q): %v", marker, err)
		}
		for _, o := range res.Objects {
			seen[o.Key]++
		}
		if !res.Truncated {
			break
		}
		marker = res.NextMarker
	}
	if len(seen) != nKeys {
		t.Fatalf("in-flight list: got %d distinct keys want %d", len(seen), nKeys)
	}
	for k, n := range seen {
		if n != 1 {
			t.Fatalf("in-flight list emitted key %q %d times — union read double-emitted a half-migrated key", k, n)
		}
	}

	// Add a fresh diverging key during the job — it has no source-layout row at
	// all, so it can only be found if writes go to the target layout AND reads
	// probe it.
	var newKey string
	for i := nKeys; i < nKeys*4; i++ {
		k := fmt.Sprintf("obj-%03d", i)
		if shardOfKey(k, activeCount) != shardOfKey(k, targetCount) {
			newKey = k
			break
		}
	}
	if newKey == "" {
		t.Fatal("could not find a diverging fresh key")
	}
	put(newKey, "new", 3)
	if got, err := store.GetObject(ctx, bucket.ID, newKey, ""); err != nil {
		t.Fatalf("in-flight get new key %s: %v", newKey, err)
	} else if got.ETag != "new" {
		t.Fatalf("in-flight get new key %s: etag=%q want new", newKey, got.ETag)
	}

	// Complete the reshard — active count rotates to the target. (In production
	// the rebalance worker calls this only after migrating every source row;
	// here we drive it directly to prove the during-job writes survive the flip.)
	if err := store.CompleteReshard(ctx, bucket.ID); err != nil {
		t.Fatalf("complete reshard: %v", err)
	}
	flipped, err := store.GetBucket(ctx, "rsh-trans")
	if err != nil {
		t.Fatalf("get bucket post-complete: %v", err)
	}
	if flipped.ShardCount != targetCount || flipped.TargetShardCount != 0 {
		t.Fatalf("post-complete layout: shard_count=%d target=%d want %d/0", flipped.ShardCount, flipped.TargetShardCount, targetCount)
	}

	// THE DISCRIMINATOR: the writes that landed during the job must survive the
	// flip. They diverge (source shard != target shard), so pre-US-002 they were
	// written to the source layout and the post-flip fnv%16 read misses them.
	if got, err := store.GetObject(ctx, bucket.ID, overwritten, ""); err != nil {
		t.Fatalf("post-flip get overwritten %s: %v (during-job write did not land in the target layout — US-002 transitional write regressed)", overwritten, err)
	} else if got.ETag != "post" {
		t.Fatalf("post-flip get overwritten %s: etag=%q want post", overwritten, got.ETag)
	}
	if got, err := store.GetObject(ctx, bucket.ID, newKey, ""); err != nil {
		t.Fatalf("post-flip get new key %s: %v (during-job write did not land in the target layout)", newKey, err)
	} else if got.ETag != "new" {
		t.Fatalf("post-flip get new key %s: etag=%q want new", newKey, got.ETag)
	}
}

// TestCassandraReshardDeleteMarkerNoResurrect proves the US-002 union read does
// not resurrect a deleted object during a reshard. A versioned object live in
// the SOURCE layout, then delete-marked DURING the job, has its marker land in
// the TARGET layout (newest version). Because the source shard still holds the
// older live version, a naive union read that swallows the cross-shard marker
// would emit the stale live row and resurrect the object. The cursor must
// surface the delete-marker head so the merge suppresses the key.
//
//   - GREEN: ListObjects omits the key; GetObject returns ErrObjectNotFound.
//   - RED (cursor swallows the marker): the source shard's live row is listed and
//     GET would return it — the object is resurrected.
func TestCassandraReshardDeleteMarkerNoResurrect(t *testing.T) {
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

	const activeCount = 8
	const targetCount = 16

	store, err := cassandra.Open(cassandra.SessionConfig{
		Hosts:       []string{host},
		Keyspace:    "strata_reshard_delmarker",
		LocalDC:     "datacenter1",
		Replication: "{'class': 'SimpleStrategy', 'replication_factor': '1'}",
		Timeout:     60 * time.Second,
	}, cassandra.Options{DefaultShardCount: activeCount})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	bucket, err := store.CreateBucket(ctx, "rsh-delmark", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	if err := store.SetBucketVersioning(ctx, "rsh-delmark", meta.VersioningEnabled); err != nil {
		t.Fatalf("enable versioning: %v", err)
	}

	// Pick a key whose source (fnv%8) and target (fnv%16) shards diverge, so the
	// live version and the delete marker land in different partitions.
	var victim string
	for i := 0; i < 10000; i++ {
		k := fmt.Sprintf("del-%04d", i)
		if shardOfKey(k, activeCount) != shardOfKey(k, targetCount) {
			victim = k
			break
		}
	}
	if victim == "" {
		t.Fatal("could not find a diverging key")
	}

	// Live version in the source layout (pre-reshard).
	if err := store.PutObject(ctx, &meta.Object{
		BucketID: bucket.ID, Key: victim,
		StorageClass: "STANDARD", ETag: "live", Size: 1,
		Mtime:    time.Now().UTC(),
		Manifest: &data.Manifest{Class: "STANDARD", Size: 1},
	}, true); err != nil {
		t.Fatalf("put live version: %v", err)
	}

	if _, err := store.StartReshard(ctx, bucket.ID, targetCount); err != nil {
		t.Fatalf("start reshard: %v", err)
	}

	// Delete during the job — the marker (newest version) lands in the target
	// layout while the live version remains in the source layout.
	if _, err := store.DeleteObject(ctx, bucket.ID, victim, "", true); err != nil {
		t.Fatalf("delete during reshard: %v", err)
	}

	// ListObjects must NOT resurrect the deleted key.
	res, err := store.ListObjects(ctx, bucket.ID, meta.ListOptions{Limit: 1000})
	if err != nil {
		t.Fatalf("list during reshard: %v", err)
	}
	for _, o := range res.Objects {
		if o.Key == victim {
			t.Fatalf("ListObjects resurrected deleted key %q during reshard (cross-shard delete marker swallowed)", victim)
		}
	}

	// GetObject latest must report the object as gone.
	if _, err := store.GetObject(ctx, bucket.ID, victim, ""); err != meta.ErrObjectNotFound {
		t.Fatalf("get deleted key %q during reshard: got %v want ErrObjectNotFound", victim, err)
	}
}

// TestCassandraReshardWorkerMovesRows is the discriminating red/green proof for
// US-003: the reshard worker must PHYSICALLY relocate each diverging row into the
// target partition (and clean the source orphan) before CompleteReshard flips the
// active count. Against the pre-US-003 skeleton (which only walked rows, never
// moved them) the post-flip point-GET of a diverging key addresses the empty
// target partition and 404s (RED). After US-003 the worker drives
// MigrateReshardKey, the row lives in the target layout, and the read resolves
// (GREEN).
//
// Coverage: a population of unversioned keys (the bulk discriminator) plus one
// versioned key with three versions (every version moves together, ordering
// preserved post-flip).
func TestCassandraReshardWorkerMovesRows(t *testing.T) {
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

	const activeCount = 64
	const targetCount = 128

	store, err := cassandra.Open(cassandra.SessionConfig{
		Hosts:       []string{host},
		Keyspace:    "strata_reshard_worker",
		LocalDC:     "datacenter1",
		Replication: "{'class': 'SimpleStrategy', 'replication_factor': '1'}",
		Timeout:     60 * time.Second,
	}, cassandra.Options{DefaultShardCount: activeCount})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	bucket, err := store.CreateBucket(ctx, "rsh-worker", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}

	// Seed unversioned keys; track which diverge between active=64 and target=128.
	const nKeys = 300
	keys := make([]string, nKeys)
	diverging := 0
	for i := 0; i < nKeys; i++ {
		keys[i] = fmt.Sprintf("obj-%04d", i)
		if err := store.PutObject(ctx, &meta.Object{
			BucketID: bucket.ID, Key: keys[i],
			StorageClass: "STANDARD", ETag: fmt.Sprintf("etag-%04d", i), Size: int64(i + 1),
			Mtime:    time.Now().UTC(),
			Manifest: &data.Manifest{Class: "STANDARD", Size: int64(i + 1)},
		}, false); err != nil {
			t.Fatalf("put %s: %v", keys[i], err)
		}
		if shardOfKey(keys[i], activeCount) != shardOfKey(keys[i], targetCount) {
			diverging++
		}
	}
	if diverging < 16 {
		t.Fatalf("only %d/%d keys diverge between active=%d and target=%d — test near-vacuous",
			diverging, nKeys, activeCount, targetCount)
	}
	t.Logf("%d/%d unversioned keys diverge under target=%d vs active=%d", diverging, nKeys, targetCount, activeCount)

	// One versioned key with three versions; choose a diverging one so the move
	// is exercised. Versioning must be enabled before the writes.
	if err := store.SetBucketVersioning(ctx, "rsh-worker", meta.VersioningEnabled); err != nil {
		t.Fatalf("enable versioning: %v", err)
	}
	var verKey string
	for i := 0; i < 100000; i++ {
		k := fmt.Sprintf("ver-%05d", i)
		if shardOfKey(k, activeCount) != shardOfKey(k, targetCount) {
			verKey = k
			break
		}
	}
	if verKey == "" {
		t.Fatal("could not find a diverging versioned key")
	}
	var verIDs []string
	for v := 0; v < 3; v++ {
		o := &meta.Object{
			BucketID: bucket.ID, Key: verKey,
			StorageClass: "STANDARD", ETag: fmt.Sprintf("v%d", v), Size: int64(10 + v),
			Mtime:    time.Now().UTC(),
			Manifest: &data.Manifest{Class: "STANDARD", Size: int64(10 + v)},
		}
		if err := store.PutObject(ctx, o, true); err != nil {
			t.Fatalf("put version %d of %s: %v", v, verKey, err)
		}
		verIDs = append(verIDs, o.VersionID)
	}

	// Kick the reshard and drive the worker to completion.
	if _, err := store.StartReshard(ctx, bucket.ID, targetCount); err != nil {
		t.Fatalf("start reshard: %v", err)
	}
	worker, err := reshard.New(reshard.Config{Meta: store, BatchLimit: 50})
	if err != nil {
		t.Fatalf("worker new: %v", err)
	}
	stats, err := worker.RunOnce(ctx)
	if err != nil {
		t.Fatalf("worker run: %v", err)
	}
	if stats.JobsCompleted != 1 {
		t.Fatalf("jobs completed: got %d want 1", stats.JobsCompleted)
	}
	// Only diverging keys move; each unversioned diverging key moves 1 row, the
	// versioned diverging key moves all 3. A skeleton worker moves 0 (RED here too).
	wantMoved := diverging + 3
	if stats.ObjectsCopied != wantMoved {
		t.Fatalf("rows moved: got %d want %d (diverging unversioned %d + 3 versions)", stats.ObjectsCopied, wantMoved, diverging)
	}

	// Active count flipped to the target; job removed.
	flipped, err := store.GetBucket(ctx, "rsh-worker")
	if err != nil {
		t.Fatalf("get bucket post-complete: %v", err)
	}
	if flipped.ShardCount != targetCount || flipped.TargetShardCount != 0 {
		t.Fatalf("post-complete layout: shard_count=%d target=%d want %d/0", flipped.ShardCount, flipped.TargetShardCount, targetCount)
	}
	if _, err := store.GetReshardJob(ctx, bucket.ID); err != meta.ErrReshardNotFound {
		t.Fatalf("post-reshard job lookup: %v", err)
	}

	// THE DISCRIMINATOR: every key must be reachable via post-flip point-GET. The
	// diverging keys' rows were physically moved into shards [64,128); without the
	// move they would 404 here.
	for i, k := range keys {
		got, err := store.GetObject(ctx, bucket.ID, k, "")
		if err != nil {
			t.Fatalf("post-flip get %s: %v (row not moved to the target partition — reshard worker did not migrate rows)", k, err)
		}
		if got.ETag != fmt.Sprintf("etag-%04d", i) {
			t.Fatalf("post-flip get %s: etag=%q want etag-%04d", k, got.ETag, i)
		}
	}

	// LIST emits each unversioned key exactly once (no duplicate from a stranded
	// source orphan).
	seen := make(map[string]int)
	marker := ""
	for {
		res, err := store.ListObjects(ctx, bucket.ID, meta.ListOptions{Limit: 1000, Marker: marker})
		if err != nil {
			t.Fatalf("post-flip list (marker=%q): %v", marker, err)
		}
		for _, o := range res.Objects {
			seen[o.Key]++
		}
		if !res.Truncated {
			break
		}
		marker = res.NextMarker
	}
	for _, k := range keys {
		if seen[k] != 1 {
			t.Fatalf("post-flip list emitted unversioned key %q %d times — stranded source orphan double-emitted", k, seen[k])
		}
	}

	// Versioned key: all three versions moved together, reachable by version id,
	// and ListObjectVersions emits each exactly once in newest-first order.
	for v, vid := range verIDs {
		got, err := store.GetObject(ctx, bucket.ID, verKey, vid)
		if err != nil {
			t.Fatalf("post-flip get %s version %s (v%d): %v (versioned row not moved)", verKey, vid, v, err)
		}
		if got.ETag != fmt.Sprintf("v%d", v) {
			t.Fatalf("post-flip get %s version %d: etag=%q want v%d", verKey, v, got.ETag, v)
		}
	}
	if got, err := store.GetObject(ctx, bucket.ID, verKey, ""); err != nil {
		t.Fatalf("post-flip get latest %s: %v", verKey, err)
	} else if got.ETag != "v2" {
		t.Fatalf("post-flip latest %s: etag=%q want v2 (latest-version ordering not preserved)", verKey, got.ETag)
	}
	vres, err := store.ListObjectVersions(ctx, bucket.ID, meta.ListOptions{Prefix: verKey, Limit: 1000})
	if err != nil {
		t.Fatalf("post-flip list versions: %v", err)
	}
	verCount := 0
	for _, o := range vres.Versions {
		if o.Key == verKey {
			verCount++
		}
	}
	if verCount != 3 {
		t.Fatalf("post-flip versioned key %q: got %d versions want 3", verKey, verCount)
	}
}
