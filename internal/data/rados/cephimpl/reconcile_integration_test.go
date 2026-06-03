//go:build integration

// In-package so the test can reach the unexported b.ioctx seam to delete a
// chunk OID out from under a manifest (the dangling-manifest condition).
package cephimpl

import (
	"bytes"
	"context"
	"crypto/rand"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/data/rados"
)

// TestChunkExistsRADOS proves the US-003b RADOS chunk prober against a live
// cluster: a chunk written by PutChunks is reported present; after its OID is
// deleted out from under the manifest (the dangling-manifest condition) the
// same probe reports absent — never an error. This is the per-OID rados stat
// the reconcile dangling pass drives via data.ChunkStater.
func TestChunkExistsRADOS(t *testing.T) {
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

	beIface, err := New(rados.Config{ConfigFile: confPath, User: user, Pool: pool, Classes: classes})
	if err != nil {
		t.Skipf("cannot connect to ceph (probably no cluster running): %v", err)
	}
	be := beIface.(*Backend)
	t.Cleanup(func() { _ = be.Close() })

	ctx := data.WithBackref(context.Background(), data.BackrefAttrs{
		BucketID:  uuid.New(),
		Key:       "reconcile/probe/object",
		VersionID: uuid.New().String(),
	})
	src := make([]byte, 5<<20) // 2 chunks: 4 + 1 MiB
	if _, err := rand.Read(src); err != nil {
		t.Fatal(err)
	}
	m, err := be.PutChunks(ctx, bytes.NewReader(src), "STANDARD")
	if err != nil {
		t.Fatalf("PutChunks: %v", err)
	}
	t.Cleanup(func() { be.cleanupManifest(context.Background(), m.Chunks) })
	if len(m.Chunks) < 2 {
		t.Fatalf("want >=2 chunks, got %d", len(m.Chunks))
	}

	// Every chunk is present.
	for _, c := range m.Chunks {
		ok, err := be.ChunkExists(context.Background(), c)
		if err != nil {
			t.Fatalf("ChunkExists(%s): %v", c.OID, err)
		}
		if !ok {
			t.Errorf("ChunkExists(%s): present chunk reported absent", c.OID)
		}
	}

	// Delete the FIRST chunk OID out from under the manifest, then re-probe:
	// it must report absent (false, nil) — the dangling-manifest signal — and
	// the surviving chunk stays present.
	victim := m.Chunks[0]
	ix, err := be.ioctx(context.Background(), victim.Cluster, victim.Pool, victim.Namespace)
	if err != nil {
		t.Fatalf("ioctx: %v", err)
	}
	if err := ix.Delete(victim.OID); err != nil {
		t.Fatalf("delete victim chunk %s: %v", victim.OID, err)
	}

	ok, err := be.ChunkExists(context.Background(), victim)
	if err != nil {
		t.Fatalf("ChunkExists(deleted %s): %v", victim.OID, err)
	}
	if ok {
		t.Errorf("ChunkExists(deleted %s): reported present after delete", victim.OID)
	}
	ok, err = be.ChunkExists(context.Background(), m.Chunks[1])
	if err != nil {
		t.Fatalf("ChunkExists(survivor): %v", err)
	}
	if !ok {
		t.Errorf("ChunkExists(survivor %s): reported absent", m.Chunks[1].OID)
	}
}
