//go:build ceph && integration

package rados_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/danchupin/strata/internal/data/rados"
)

// TestRADOSBackend exercises the full PutChunks → GetChunks → Delete lifecycle
// against a live Ceph cluster. Requires a readable ceph.conf at either
// STRATA_TEST_CEPH_CONF or /etc/ceph/ceph.conf, plus the usual classes pool.
// Skipped when no config is reachable so the build stays green outside of CI.
//
// Runs only under `go test -tags "ceph integration"`.
func TestRADOSBackend(t *testing.T) {
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

	be, err := rados.New(rados.Config{
		ConfigFile: confPath,
		User:       envOr("STRATA_TEST_CEPH_USER", "admin"),
		Pool:       pool,
		Classes:    classes,
	})
	if err != nil {
		t.Skipf("cannot connect to ceph (probably no cluster running): %v", err)
	}
	t.Cleanup(func() { _ = be.Close() })

	t.Run("PutGetDelete", func(t *testing.T) {
		ctx := context.Background()
		src := make([]byte, 9<<20) // 9 MiB → 3 chunks of 4+4+1 MiB
		if _, err := rand.Read(src); err != nil {
			t.Fatal(err)
		}
		manifest, err := be.PutChunks(ctx, bytes.NewReader(src), "STANDARD")
		if err != nil {
			t.Fatalf("PutChunks: %v", err)
		}
		if manifest.Size != int64(len(src)) {
			t.Errorf("manifest.Size: got %d want %d", manifest.Size, len(src))
		}
		if len(manifest.Chunks) != 3 {
			t.Errorf("chunk count: got %d want 3 (4+4+1 MiB)", len(manifest.Chunks))
		}
		for i, c := range manifest.Chunks {
			if c.Pool != pool {
				t.Errorf("chunk %d wrong pool: got %q want %q", i, c.Pool, pool)
			}
			if c.OID == "" {
				t.Errorf("chunk %d has empty OID", i)
			}
		}

		rc, err := be.GetChunks(ctx, manifest, 0, manifest.Size)
		if err != nil {
			t.Fatalf("GetChunks: %v", err)
		}
		got, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(src, got) {
			t.Fatalf("round-trip byte mismatch: %d != %d", len(src), len(got))
		}

		if err := be.Delete(ctx, manifest); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		// Second delete on the same manifest should be a no-op (ENOENT-tolerant path).
		if err := be.Delete(ctx, manifest); err != nil {
			t.Logf("second Delete returned %v (usually ENOENT, fine)", err)
		}
	})

	t.Run("RangeRead", func(t *testing.T) {
		ctx := context.Background()
		src := []byte("0123456789abcdef")
		manifest, err := be.PutChunks(ctx, bytes.NewReader(src), "STANDARD")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = be.Delete(ctx, manifest) })

		rc, err := be.GetChunks(ctx, manifest, 4, 6)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "456789" {
			t.Errorf("range 4-9: got %q want %q", got, "456789")
		}
	})

	t.Run("UnknownClassRejected", func(t *testing.T) {
		_, err := be.PutChunks(context.Background(), bytes.NewReader([]byte("x")), "NOT_A_CLASS")
		if err == nil || !strings.Contains(err.Error(), "unknown storage class") {
			t.Errorf("expected unknown-class error, got %v", err)
		}
	})
}

func envOr(k, def string) string {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		return v
	}
	return def
}
