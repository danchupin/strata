//go:build integration

// In-package (not cephimpl_test) so the test can reach the unexported
// b.ioctx seam to read the back-reference xattr straight off the chunk OID.
package cephimpl

import (
	"bytes"
	"context"
	"crypto/rand"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/data/rados"
)

// TestPutChunksStampsBackrefXattr proves US-001 end-to-end against a live
// Ceph cluster: PutChunks with a back-reference identity on ctx stamps
// user.strata.backref on EVERY chunk in the same WriteOp as the body, and
// the decoded xattr carries the exact {bucket, key, version, chunk_idx,
// mtime} — with chunk_idx incrementing per chunk. Skipped when no cluster
// is reachable (matches TestRADOSBackend's guard).
func TestPutChunksStampsBackrefXattr(t *testing.T) {
	confPath := os.Getenv("STRATA_TEST_CEPH_CONF")
	if confPath == "" {
		confPath = "/etc/ceph/ceph.conf"
	}
	if _, err := os.Stat(confPath); err != nil {
		t.Skipf("ceph config not reachable at %s: %v", confPath, err)
	}
	pool := os.Getenv("STRATA_TEST_CEPH_POOL")
	if pool == "" {
		pool = "strata.rgw.buckets.data"
	}
	classesEnv := os.Getenv("STRATA_TEST_CEPH_CLASSES")
	if classesEnv == "" {
		classesEnv = "STANDARD=" + pool
	}
	classes, err := rados.ParseClasses(classesEnv)
	if err != nil {
		t.Fatalf("parse classes %q: %v", classesEnv, err)
	}
	user := os.Getenv("STRATA_TEST_CEPH_USER")
	if user == "" {
		user = "admin"
	}

	t.Setenv("STRATA_CHUNK_BACKREF", "true")
	beIface, err := New(rados.Config{ConfigFile: confPath, User: user, Pool: pool, Classes: classes})
	if err != nil {
		t.Skipf("cannot connect to ceph (probably no cluster running): %v", err)
	}
	be := beIface.(*Backend)
	t.Cleanup(func() { _ = be.Close() })

	bid := uuid.New()
	ver := uuid.New().String()
	const key = "backref/test/object"
	mtime := time.Unix(1717000000, 0).UTC()
	ctx := data.WithBackref(context.Background(), data.BackrefAttrs{
		BucketID:  bid,
		Key:       key,
		VersionID: ver,
		Mtime:     mtime,
	})

	src := make([]byte, 9<<20) // 3 chunks: 4 + 4 + 1 MiB
	if _, err := rand.Read(src); err != nil {
		t.Fatal(err)
	}
	m, err := be.PutChunks(ctx, bytes.NewReader(src), "STANDARD")
	if err != nil {
		t.Fatalf("PutChunks: %v", err)
	}
	t.Cleanup(func() { be.cleanupManifest(context.Background(), m.Chunks) })
	if len(m.Chunks) != 3 {
		t.Fatalf("want 3 chunks, got %d", len(m.Chunks))
	}

	for i, c := range m.Chunks {
		ix, err := be.ioctx(context.Background(), c.Cluster, c.Pool, c.Namespace)
		if err != nil {
			t.Fatalf("ioctx chunk %d: %v", i, err)
		}
		buf := make([]byte, 4096)
		n, err := ix.GetXattr(c.OID, data.BackrefXattrName, buf)
		if err != nil {
			t.Fatalf("GetXattr chunk %d (%s): %v", i, c.OID, err)
		}
		ref, err := data.DecodeBackref(buf[:n])
		if err != nil {
			t.Fatalf("DecodeBackref chunk %d: %v", i, err)
		}
		if ref.BucketID != bid {
			t.Errorf("chunk %d BucketID: want %s, got %s", i, bid, ref.BucketID)
		}
		if ref.Key != key {
			t.Errorf("chunk %d Key: want %q, got %q", i, key, ref.Key)
		}
		if ref.VersionID != ver {
			t.Errorf("chunk %d VersionID: want %s, got %s", i, ver, ref.VersionID)
		}
		if ref.ChunkIdx != i {
			t.Errorf("chunk %d ChunkIdx: want %d, got %d", i, i, ref.ChunkIdx)
		}
		if !ref.Mtime.Equal(mtime) {
			t.Errorf("chunk %d Mtime: want %v, got %v", i, mtime, ref.Mtime)
		}
	}

	// US-002: EnumeratePool with WithBackref surfaces the same xattr inline so
	// the reconcile worker attributes each chunk without a second round trip.
	expectedOIDs := make(map[string]bool, len(m.Chunks))
	for _, c := range m.Chunks {
		expectedOIDs[c.OID] = true
	}
	seen := make(map[string]data.Backref, len(m.Chunks))
	err = rados.EnumeratePool(ctx, beIface, rados.DefaultCluster,
		rados.EnumerateOptions{
			Pool:          pool,
			Namespace:     rados.EnumerateAllNamespaces,
			ChunkOIDsOnly: true,
			WithBackref:   true,
		},
		func(o rados.PoolObject, _ rados.EnumerateCursor) error {
			if !expectedOIDs[o.OID] {
				return nil // some other test/object sharing the pool
			}
			if len(o.Backref) == 0 {
				t.Errorf("WithBackref: chunk %s carried no back-reference bytes", o.OID)
				return nil
			}
			br, derr := data.DecodeBackref(o.Backref)
			if derr != nil {
				t.Errorf("WithBackref: decode %s: %v", o.OID, derr)
				return nil
			}
			seen[o.OID] = br
			return nil
		})
	if err != nil {
		t.Fatalf("EnumeratePool WithBackref: %v", err)
	}
	for _, c := range m.Chunks {
		br, ok := seen[c.OID]
		if !ok {
			t.Errorf("WithBackref: chunk %s not enumerated", c.OID)
			continue
		}
		if br.BucketID != bid || br.Key != key || br.VersionID != ver {
			t.Errorf("WithBackref: chunk %s identity mismatch: %+v", c.OID, br)
		}
	}
}
