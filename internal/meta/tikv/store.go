// Package tikv is the TiKV-backed implementation of meta.Store.
//
// US-001 lands the skeleton: every method is a stub returning
// errors.ErrUnsupported so the package compiles and satisfies the
// meta.Store interface contract while subsequent stories fill in the
// real implementations (key encoding US-002, bucket CRUD US-003, ...).
//
// STRATA_META_BACKEND=tikv is reserved but NOT yet wired into
// internal/serverapp's dispatch — production routing lands in US-015.
package tikv

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/meta"
)

// Config holds connection parameters for a TiKV cluster. Only the PD
// (Placement Driver) endpoint list is required; later stories may add
// TLS, timeouts, retry knobs.
type Config struct {
	PDEndpoints []string
}

// Store is the TiKV-backed meta.Store. Concrete behaviour lives in
// per-section files (buckets.go, ...); this file holds construction +
// cross-cutting plumbing.
type Store struct {
	cfg Config
	kv  kvBackend
}

// Open dials the cluster identified by cfg.PDEndpoints and returns a Store
// ready for use. Use openWithBackend (test-only) to inject the in-process
// memBackend.
func Open(cfg Config) (*Store, error) {
	b, err := newTiKVBackend(cfg.PDEndpoints)
	if err != nil {
		return nil, err
	}
	return &Store{cfg: cfg, kv: b}, nil
}

// openWithBackend builds a Store backed by the supplied kvBackend. Used by
// unit tests to inject memBackend without dialing PD.
func openWithBackend(b kvBackend) *Store {
	return &Store{kv: b}
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

func (s *Store) SetBucketGrants(ctx context.Context, bucketID uuid.UUID, grants []meta.Grant) error {
	return errors.ErrUnsupported
}

func (s *Store) GetBucketGrants(ctx context.Context, bucketID uuid.UUID) ([]meta.Grant, error) {
	return nil, errors.ErrUnsupported
}

func (s *Store) DeleteBucketGrants(ctx context.Context, bucketID uuid.UUID) error {
	return errors.ErrUnsupported
}

func (s *Store) SetObjectGrants(ctx context.Context, bucketID uuid.UUID, key, versionID string, grants []meta.Grant) error {
	return errors.ErrUnsupported
}

func (s *Store) GetObjectGrants(ctx context.Context, bucketID uuid.UUID, key, versionID string) ([]meta.Grant, error) {
	return nil, errors.ErrUnsupported
}

func (s *Store) SetObjectTags(ctx context.Context, bucketID uuid.UUID, key, versionID string, tags map[string]string) error {
	return errors.ErrUnsupported
}

func (s *Store) GetObjectTags(ctx context.Context, bucketID uuid.UUID, key, versionID string) (map[string]string, error) {
	return nil, errors.ErrUnsupported
}

func (s *Store) DeleteObjectTags(ctx context.Context, bucketID uuid.UUID, key, versionID string) error {
	return errors.ErrUnsupported
}

func (s *Store) SetObjectRetention(ctx context.Context, bucketID uuid.UUID, key, versionID, mode string, until time.Time) error {
	return errors.ErrUnsupported
}

func (s *Store) SetObjectLegalHold(ctx context.Context, bucketID uuid.UUID, key, versionID string, on bool) error {
	return errors.ErrUnsupported
}

func (s *Store) SetObjectRestoreStatus(ctx context.Context, bucketID uuid.UUID, key, versionID, status string) error {
	return errors.ErrUnsupported
}

func (s *Store) CreateAccessPoint(ctx context.Context, ap *meta.AccessPoint) error {
	return errors.ErrUnsupported
}

func (s *Store) GetAccessPoint(ctx context.Context, name string) (*meta.AccessPoint, error) {
	return nil, errors.ErrUnsupported
}

func (s *Store) GetAccessPointByAlias(ctx context.Context, alias string) (*meta.AccessPoint, error) {
	return nil, errors.ErrUnsupported
}

func (s *Store) DeleteAccessPoint(ctx context.Context, name string) error {
	return errors.ErrUnsupported
}

func (s *Store) ListAccessPoints(ctx context.Context, bucketID uuid.UUID) ([]*meta.AccessPoint, error) {
	return nil, errors.ErrUnsupported
}

func (s *Store) UpdateObjectSSEWrap(ctx context.Context, bucketID uuid.UUID, key, versionID string, wrapped []byte, keyID string) error {
	return errors.ErrUnsupported
}

func (s *Store) SetRewrapProgress(ctx context.Context, p *meta.RewrapProgress) error {
	return errors.ErrUnsupported
}

func (s *Store) GetRewrapProgress(ctx context.Context, bucketID uuid.UUID) (*meta.RewrapProgress, error) {
	return nil, errors.ErrUnsupported
}

func (s *Store) GetObjectManifestRaw(ctx context.Context, bucketID uuid.UUID, key, versionID string) ([]byte, error) {
	return nil, errors.ErrUnsupported
}

func (s *Store) UpdateObjectManifestRaw(ctx context.Context, bucketID uuid.UUID, key, versionID string, raw []byte) error {
	return errors.ErrUnsupported
}

func (s *Store) SetObjectReplicationStatus(ctx context.Context, bucketID uuid.UUID, key, versionID, status string) error {
	return errors.ErrUnsupported
}

// Compile-time guarantees that *Store satisfies both meta.Store and the
// optional meta.RangeScanStore capability surface (US-012). Stories that
// touch either interface should preserve these assertions.
var (
	_ meta.Store          = (*Store)(nil)
	_ meta.RangeScanStore = (*Store)(nil)
)
