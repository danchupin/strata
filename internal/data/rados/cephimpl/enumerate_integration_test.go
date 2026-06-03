//go:build integration

package cephimpl_test

import (
	"context"
	"fmt"
	"os"
	"sort"
	"testing"

	"github.com/google/uuid"

	"github.com/danchupin/strata/cephimpl"
	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/data/rados"
)

// TestEnumeratePool seeds a known set of chunk OIDs (plus foreign objects)
// into an isolated namespace of the test pool, then proves the pool walk:
//
//	exact   — ChunkOIDsOnly enumeration returns EXACTLY the seeded chunk
//	          OIDs (foreign objects skipped, none dropped, none duplicated).
//	resume  — a fresh walk seeked to a mid-walk cursor drops nothing past
//	          that cursor (at-least-once; PG-hash granularity tolerated by
//	          deduping). Union with the pre-cursor prefix covers all N.
//
// Skipped outside CI when no ceph cluster is reachable (matches
// TestRADOSBackend's guard).
func TestEnumeratePool(t *testing.T) {
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
	classes, err := rados.ParseClasses("STANDARD=" + pool)
	if err != nil {
		t.Fatalf("parse classes: %v", err)
	}
	be, err := cephimpl.New(rados.Config{
		ConfigFile: confPath,
		User:       envOr("STRATA_TEST_CEPH_USER", "admin"),
		Pool:       pool,
		Classes:    classes,
	})
	if err != nil {
		t.Skipf("cannot connect to ceph: %v", err)
	}
	t.Cleanup(func() { _ = be.Close() })

	// Isolate this run in its own namespace so a shared pool's pre-existing
	// objects never bleed into the assertions.
	ns := "strata-reconcile-it-" + uuid.NewString()
	backend, ok := be.(*cephimpl.Backend)
	if !ok {
		t.Fatalf("cephimpl.New returned %T, want *cephimpl.Backend", be)
	}
	clusters := cephimpl.RebalanceClusters(backend)
	cluster := clusters[rados.DefaultCluster]
	if cluster == nil {
		t.Fatalf("no %q cluster facade; got %v", rados.DefaultCluster, clusters)
	}

	ctx := context.Background()
	const n = 12
	objID := uuid.NewString()
	chunkOIDs := make([]string, 0, n)
	for i := 0; i < n; i++ {
		oid := fmt.Sprintf("%s.%05d", objID, i)
		chunkOIDs = append(chunkOIDs, oid)
		if err := cluster.Write(ctx, pool, ns, oid, []byte("chunk")); err != nil {
			t.Fatalf("seed chunk %s: %v", oid, err)
		}
	}
	// Foreign objects: NOT shaped like a Strata chunk OID — must be skipped.
	foreign := []string{"rgw.meta.index.shard", "bilog.0001", objID + ".bad"}
	for _, oid := range foreign {
		if err := cluster.Write(ctx, pool, ns, oid, []byte("x")); err != nil {
			t.Fatalf("seed foreign %s: %v", oid, err)
		}
	}
	t.Cleanup(func() {
		refs := make([]data.ChunkRef, 0, n+len(foreign))
		for _, oid := range append(append([]string{}, chunkOIDs...), foreign...) {
			refs = append(refs, data.ChunkRef{Cluster: rados.DefaultCluster, Pool: pool, Namespace: ns, OID: oid})
		}
		_ = be.Delete(context.Background(), &data.Manifest{Chunks: refs})
	})

	// --- exact: ChunkOIDsOnly returns precisely the seeded chunk OIDs. ---
	type step struct {
		oid    string
		cursor rados.EnumerateCursor
	}
	var walk []step
	err = rados.EnumeratePool(ctx, be, rados.DefaultCluster,
		rados.EnumerateOptions{Pool: pool, Namespace: ns, ChunkOIDsOnly: true},
		func(o rados.PoolObject, c rados.EnumerateCursor) error {
			walk = append(walk, step{oid: o.OID, cursor: c})
			return nil
		})
	if err != nil {
		t.Fatalf("EnumeratePool (exact): %v", err)
	}
	gotOIDs := make([]string, len(walk))
	for i, s := range walk {
		gotOIDs[i] = s.oid
	}
	if !sameSet(gotOIDs, chunkOIDs) {
		t.Fatalf("exact walk mismatch:\n got  %v\n want %v", sortedCopy(gotOIDs), sortedCopy(chunkOIDs))
	}
	if len(gotOIDs) != n {
		t.Fatalf("exact walk emitted %d OIDs (dup?), want %d", len(gotOIDs), n)
	}

	// --- resume: seek to a mid-walk cursor, assert no drop past it. ---
	mid := len(walk) / 2
	resumeCursor := walk[mid].cursor
	seen := make(map[string]bool)
	err = rados.EnumeratePool(ctx, be, rados.DefaultCluster,
		rados.EnumerateOptions{Pool: pool, Namespace: ns, ChunkOIDsOnly: true, Start: resumeCursor},
		func(o rados.PoolObject, _ rados.EnumerateCursor) error {
			seen[o.OID] = true
			return nil
		})
	if err != nil {
		t.Fatalf("EnumeratePool (resume): %v", err)
	}
	// Everything strictly AFTER the resume cursor must reappear (no drop).
	for _, s := range walk[mid+1:] {
		if !seen[s.oid] {
			t.Fatalf("resume from cursor %d dropped %s", resumeCursor, s.oid)
		}
	}
	// Prefix ∪ resumed, deduped, must cover the full seeded set (no drop).
	union := make(map[string]bool)
	for _, s := range walk[:mid+1] {
		union[s.oid] = true
	}
	for oid := range seen {
		union[oid] = true
	}
	for _, oid := range chunkOIDs {
		if !union[oid] {
			t.Fatalf("prefix∪resume missing seeded chunk %s", oid)
		}
	}
	// Forward-progress proof: when the resume cursor sits past the first
	// object's PG (cursor differs), Seek MUST have advanced — the resumed
	// walk emits STRICTLY FEWER than n objects. Without this a Seek that
	// silently no-op'd back to 0 (replaying everything) would still satisfy
	// the no-drop checks above. Guarded on the cursor differing so a
	// single-PG pool (all cursors equal → full replay) does not false-fail.
	if resumeCursor != walk[0].cursor && len(seen) >= n {
		t.Fatalf("resume from advanced cursor %d emitted %d OIDs, want < %d (Seek did not advance)",
			resumeCursor, len(seen), n)
	}
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	as, bs := sortedCopy(a), sortedCopy(b)
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}

func sortedCopy(in []string) []string {
	out := append([]string{}, in...)
	sort.Strings(out)
	return out
}
