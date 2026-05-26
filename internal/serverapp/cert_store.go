package serverapp

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
)

// certStore is an atomic.Pointer-backed SNI dispatcher used by US-003
// hot-reloadable TLS. Reads (the per-handshake GetCertificate callback)
// are lock-free: a single atomic.Pointer.Load yields the current snapshot,
// which is then immutable for the lifetime of any in-flight handshake.
// Writers (reload) build a fresh snapshot and Swap the pointer; the old
// snapshot remains referenced by in-flight handshakes until Go GC reclaims
// it.
//
// snapshot.fallback covers the default cert returned when the client
// omits SNI or the requested ServerName has no matching SAN/CN entry.
// snapshot.byName indexes SAN DNSNames + Subject CN for every loaded
// pair, lowercased — TLS ServerName is canonically lowercase per RFC
// 6066.
type certStore struct {
	ptr atomic.Pointer[certSnapshot]
}

type certSnapshot struct {
	fallback *tls.Certificate
	byName   map[string]*tls.Certificate
	pairs    []loadedPair
}

// loadedPair is the on-disk evidence backing one parsed cert + key
// (single-cert: certFile/keyFile; cert-dir: each *.crt + matching
// *.key). The reloader stats these paths on every reconcile tick and
// compares against the cached fingerprint to decide whether to rebuild
// the snapshot.
type loadedPair struct {
	certPath string
	keyPath  string
	fp       fileFingerprint
	cert     *tls.Certificate
}

// fileFingerprint captures stat-derived identity for change detection.
// (size, mtimeNanos, inode/dev) is sufficient: cert-manager + k8s
// atomic-symlink swaps replace the underlying file so at least one field
// flips; in-place writes flip mtime.
type fileFingerprint struct {
	certSize  int64
	certMtime int64
	keySize   int64
	keyMtime  int64
}

func (s *certStore) load() *certSnapshot {
	return s.ptr.Load()
}

func (s *certStore) swap(next *certSnapshot) {
	s.ptr.Store(next)
}

// GetCertificate implements the tls.Config.GetCertificate callback.
// chi.ServerName is empty for clients that omit SNI — we fall back to
// the first loaded pair.
func (s *certStore) GetCertificate(chi *tls.ClientHelloInfo) (*tls.Certificate, error) {
	snap := s.load()
	if snap == nil {
		return nil, errors.New("cert store: empty snapshot")
	}
	name := strings.ToLower(strings.TrimSpace(chi.ServerName))
	if name != "" {
		if c, ok := snap.byName[name]; ok {
			return c, nil
		}
		// Wildcard match: *.example.com matches foo.example.com.
		if dot := strings.IndexByte(name, '.'); dot >= 0 {
			if c, ok := snap.byName["*"+name[dot:]]; ok {
				return c, nil
			}
		}
	}
	if snap.fallback != nil {
		return snap.fallback, nil
	}
	return nil, fmt.Errorf("cert store: no certificate for ServerName=%q", chi.ServerName)
}

// buildSnapshotFromDir walks dir for *.crt + matching *.key pairs and
// returns a fresh snapshot. Pairs missing a matching key are skipped
// (logged by the caller). Returns an error only when dir cannot be
// read; an empty directory yields an empty snapshot which the caller
// rejects.
func buildSnapshotFromDir(dir string) (*certSnapshot, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read tls cert dir %s: %w", dir, err)
	}
	snap := &certSnapshot{byName: map[string]*tls.Certificate{}}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".crt") {
			continue
		}
		certPath := filepath.Join(dir, name)
		keyPath := filepath.Join(dir, strings.TrimSuffix(name, ".crt")+".key")
		pair, err := loadPair(certPath, keyPath)
		if err != nil {
			return nil, fmt.Errorf("load cert pair %s: %w", name, err)
		}
		snap.indexPair(pair)
	}
	if len(snap.pairs) == 0 {
		return nil, fmt.Errorf("tls cert dir %s: no *.crt/*.key pairs found", dir)
	}
	snap.fallback = snap.pairs[0].cert
	return snap, nil
}

// buildSnapshotFromSingle loads a single cert + key pair into a fresh
// snapshot. Empty paths yield (nil, nil) — caller falls back to plain
// HTTP.
func buildSnapshotFromSingle(certPath, keyPath string) (*certSnapshot, error) {
	if certPath == "" {
		return nil, nil
	}
	pair, err := loadPair(certPath, keyPath)
	if err != nil {
		return nil, err
	}
	snap := &certSnapshot{byName: map[string]*tls.Certificate{}}
	snap.indexPair(pair)
	snap.fallback = pair.cert
	return snap, nil
}

func loadPair(certPath, keyPath string) (loadedPair, error) {
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return loadedPair{}, fmt.Errorf("tls load key pair %s: %w", certPath, err)
	}
	if len(cert.Certificate) > 0 {
		leaf, err := x509.ParseCertificate(cert.Certificate[0])
		if err == nil {
			cert.Leaf = leaf
		}
	}
	fp, err := fingerprintPair(certPath, keyPath)
	if err != nil {
		return loadedPair{}, err
	}
	return loadedPair{
		certPath: certPath,
		keyPath:  keyPath,
		fp:       fp,
		cert:     &cert,
	}, nil
}

func fingerprintPair(certPath, keyPath string) (fileFingerprint, error) {
	cs, err := os.Stat(certPath)
	if err != nil {
		return fileFingerprint{}, err
	}
	ks, err := os.Stat(keyPath)
	if err != nil {
		return fileFingerprint{}, err
	}
	return fileFingerprint{
		certSize:  cs.Size(),
		certMtime: cs.ModTime().UnixNano(),
		keySize:   ks.Size(),
		keyMtime:  ks.ModTime().UnixNano(),
	}, nil
}

// indexPair maps every SAN DNSName + Subject CN onto the cert and
// appends the pair to snap.pairs.
func (s *certSnapshot) indexPair(p loadedPair) {
	s.pairs = append(s.pairs, p)
	if p.cert.Leaf == nil {
		return
	}
	for _, dns := range p.cert.Leaf.DNSNames {
		s.byName[strings.ToLower(dns)] = p.cert
	}
	if cn := p.cert.Leaf.Subject.CommonName; cn != "" {
		s.byName[strings.ToLower(cn)] = p.cert
	}
}
