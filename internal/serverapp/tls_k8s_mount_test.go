//go:build integration

package serverapp

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestK8sSecretMountAtomicRotation drives an end-to-end TLS hot-reload
// through the kubelet Secret-mount layout:
//
//	mount/
//	  ..2026_05_26_11_00_00.v1/
//	    tls.crt
//	    tls.key
//	  ..2026_05_26_11_05_00.v2/
//	    tls.crt
//	    tls.key
//	  ..data -> ..2026_05_26_11_00_00.v1
//	  tls.crt -> ..data/tls.crt
//	  tls.key -> ..data/tls.key
//
// Cert-manager rotates by writing a v3 dir, swapping the `..data`
// symlink atomically (via rename), and pruning the old version dir.
// The reloader watches the parent of `..data/tls.crt` (which resolves
// to the mount root) and treats `..data` events as cert-changed
// signals. Periodic 100ms reconciliation absorbs any race on the
// fsnotify watcher.
func TestK8sSecretMountAtomicRotation(t *testing.T) {
	mount := t.TempDir()
	v1 := filepath.Join(mount, "..2026_05_26_11_00_00.v1")
	v2 := filepath.Join(mount, "..2026_05_26_11_05_00.v2")
	if err := os.Mkdir(v1, 0o755); err != nil {
		t.Fatalf("mkdir v1: %v", err)
	}
	if err := os.Mkdir(v2, 0o755); err != nil {
		t.Fatalf("mkdir v2: %v", err)
	}
	writeSelfSignedCertAt(t, v1, "tls", "service.example.com")
	writeSelfSignedCertAt(t, v2, "tls", "service.example.com")
	if err := os.Symlink(v1, filepath.Join(mount, "..data")); err != nil {
		t.Fatalf("symlink ..data: %v", err)
	}
	// Outer symlinks tls.crt/tls.key -> ..data/tls.crt that real
	// kubelet projects expose; the certFile path the operator gives
	// the gateway is the outer link.
	certPath := filepath.Join(mount, "tls.crt")
	keyPath := filepath.Join(mount, "tls.key")
	if err := os.Symlink(filepath.Join("..data", "tls.crt"), certPath); err != nil {
		t.Fatalf("outer symlink tls.crt: %v", err)
	}
	if err := os.Symlink(filepath.Join("..data", "tls.key"), keyPath); err != nil {
		t.Fatalf("outer symlink tls.key: %v", err)
	}

	store := &certStore{}
	initial, err := buildSnapshotFromSingle(certPath, keyPath)
	if err != nil {
		t.Fatalf("buildSnapshotFromSingle: %v", err)
	}
	store.swap(initial)
	beforeFP := initial.pairs[0].fp
	beforeLeaf := snapshotLeafBytes(t, store)

	r := &certReloader{
		store:    store,
		logger:   discardLogger(),
		certFile: certPath,
		keyFile:  keyPath,
		interval: 100 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go r.run(ctx)
	time.Sleep(250 * time.Millisecond)

	// cert-manager-shape atomic swap: create tmp link to v2, rename
	// onto ..data.
	tmpLink := filepath.Join(mount, "..data_tmp")
	if err := os.Symlink(v2, tmpLink); err != nil {
		t.Fatalf("symlink v2: %v", err)
	}
	if err := os.Rename(tmpLink, filepath.Join(mount, "..data")); err != nil {
		t.Fatalf("rename swap: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		current := store.load()
		if len(current.pairs) > 0 && current.pairs[0].fp != beforeFP {
			afterLeaf := snapshotLeafBytes(t, store)
			if string(afterLeaf) == string(beforeLeaf) {
				t.Fatal("snapshot fingerprint flipped but leaf bytes match — reload didn't actually load v2 cert")
			}
			// Sanity check: GetCertificate returns the v2 leaf.
			cert, err := r.store.GetCertificate(&tls.ClientHelloInfo{ServerName: "service.example.com"})
			if err != nil {
				t.Fatalf("GetCertificate post-swap: %v", err)
			}
			if string(cert.Certificate[0]) != string(afterLeaf) {
				t.Error("GetCertificate returned stale cert post-swap")
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("k8s atomic rotation never observed (fsnotify + 100ms periodic both missed)")
}

func snapshotLeafBytes(t *testing.T, store *certStore) []byte {
	t.Helper()
	snap := store.load()
	if snap == nil || len(snap.pairs) == 0 {
		t.Fatal("snapshot empty")
	}
	der := snap.pairs[0].cert.Certificate[0]
	if _, err := x509.ParseCertificate(der); err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	return der
}
