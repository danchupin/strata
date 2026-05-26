// Package tikv is the TiKV-backed implementation of meta.Store.
//
// Concrete behaviour lives in per-section files (buckets.go, objects.go,
// blobs.go, sse_rewrap.go, ...); this file holds construction + cross-
// cutting plumbing (Config / Open / Close / Probe).
package tikv

import (
	"context"
	"errors"

	"go.opentelemetry.io/otel/trace"

	"github.com/danchupin/strata/internal/meta"
)

// Config holds connection parameters for a TiKV cluster. Only the PD
// (Placement Driver) endpoint list is required; later stories may add
// TLS, timeouts, retry knobs.
type Config struct {
	PDEndpoints []string
	// GCDualWrite controls whether EnqueueChunkDeletion fan-outs to both
	// the legacy `s/qg/...` prefix and the new `s/qG/...` shard-aware
	// prefix during the US-003 cutover. nil → fall back to
	// GCDualWriteFromEnv(); explicit *bool wins. Mirrors
	// cassandra.Options.GCDualWrite.
	GCDualWrite *bool
	// Tracer drives the per-op meta.tikv.<table>.<op> span emission. Nil
	// disables span emission (production paths set it via
	// serverapp.buildMetaStore; tests leave it nil). Mirrors
	// cassandra.SessionConfig.Tracer.
	Tracer trace.Tracer
	// Metrics receives optional per-op signals (currently the per-shard
	// BucketStats counter — US-002 p1-fixes). Nil disables counters; production
	// paths wire metrics.TiKVObserver{} via serverapp.buildMetaStore.
	Metrics Metrics
	// TLS wires tikv-client-go config.Security at Open time when any field
	// is set (US-005 harden-gateway). Empty CAFile + CertFile + KeyFile +
	// SkipVerify=false = plain-gRPC = current backwards-compat behavior.
	// CertFile + KeyFile must come as a pair; CAFile populates RootCAs.
	// SkipVerify=true logs a WARN at boot via the caller (serverapp) and
	// is honored on the PD HTTP control plane; the gRPC data plane is bound
	// to tikv-client-go's Security.ToTLSConfig() (does NOT expose
	// InsecureSkipVerify — library limitation).
	TLS TLSConfig
}

// TLSConfig is the subset of TiKVTLSConfig consumed by the tikv backend.
// The serverapp layer translates internal/config.TiKVTLSConfig →
// tikv.TLSConfig at the boundary so this package never imports
// internal/config.
type TLSConfig struct {
	CAFile     string
	CertFile   string
	KeyFile    string
	SkipVerify bool
}

// HasAny reports whether any TLS knob is set. Used by the dial path to
// decide between plain-gRPC (zero value) and Security-wired TLS.
func (t TLSConfig) HasAny() bool {
	return t.CAFile != "" || t.CertFile != "" || t.KeyFile != "" || t.SkipVerify
}

// WithGCDualWrite is a helper for tests / callers that want to set the
// optional pointer cleanly: `tikv.Config{..., GCDualWrite: tikv.WithGCDualWrite(false)}`.
func WithGCDualWrite(v bool) *bool { return &v }

// Store is the TiKV-backed meta.Store. Concrete behaviour lives in
// per-section files (buckets.go, ...); this file holds construction +
// cross-cutting plumbing.
type Store struct {
	cfg         Config
	kv          kvBackend
	gcDualWrite bool
	observer    *Observer
	metrics     Metrics
}

// Open dials the cluster identified by cfg.PDEndpoints and returns a Store
// ready for use. Use openWithBackend (test-only) to inject the in-process
// memBackend.
func Open(cfg Config) (*Store, error) {
	b, err := newTiKVBackend(cfg.PDEndpoints, cfg.TLS)
	if err != nil {
		return nil, err
	}
	gcDualWrite := GCDualWriteFromEnv()
	if cfg.GCDualWrite != nil {
		gcDualWrite = *cfg.GCDualWrite
	}
	return &Store{cfg: cfg, kv: b, gcDualWrite: gcDualWrite, observer: NewObserver(cfg.Tracer), metrics: cfg.Metrics}, nil
}

// openWithBackend builds a Store backed by the supplied kvBackend. Used by
// unit tests to inject memBackend without dialing PD.
func openWithBackend(b kvBackend) *Store {
	return &Store{kv: b, gcDualWrite: GCDualWriteFromEnv()}
}

// openWithBackendAndObserver mirrors openWithBackend but also wires a
// per-op Observer. Used by observer_test.go to verify span emission against
// the in-process memBackend.
func openWithBackendAndObserver(b kvBackend, o *Observer) *Store {
	return &Store{kv: b, gcDualWrite: GCDualWriteFromEnv(), observer: o}
}

// Close releases the underlying kv connection.
func (s *Store) Close() error {
	if s.kv == nil {
		return nil
	}
	return s.kv.Close()
}

// Probe is the readiness probe consumed by the gateway /readyz endpoint
// (see internal/health.Handler wiring in serverapp).
func (s *Store) Probe(ctx context.Context) error {
	if s == nil || s.kv == nil {
		return errors.New("tikv: store not opened")
	}
	return s.kv.Probe(ctx)
}

// Compile-time guarantees that *Store satisfies both meta.Store and the
// optional meta.RangeScanStore capability surface (US-012). Stories that
// touch either interface should preserve these assertions.
var (
	_ meta.Store          = (*Store)(nil)
	_ meta.RangeScanStore = (*Store)(nil)
)
