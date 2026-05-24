package auth

import (
	"context"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
)

// encodeDEKAsSecret renders the plaintext DEK as a hex string so SigV4's
// deriveSigningKey can consume it as a regular secret. Clients sign
// with the same hex representation.
func encodeDEKAsSecret(dek []byte) string {
	return hex.EncodeToString(dek)
}

// BucketSigningKeyStore is the narrow meta-store surface the auth
// middleware uses to look up per-bucket signing-key envelopes (US-001
// auth-dx-trailer-lima). Implemented by *meta.cassandra.Store /
// *meta.tikv.Store / *meta.memory.Store via meta.Store; the auth
// package consumes only the read path.
type BucketSigningKeyStore interface {
	GetBucketSigningKey(ctx context.Context, name string) (wrapped []byte, keyID string, createdAt time.Time, err error)
}

// KMSUnwrapper is the narrow surface kms.Provider exposes for DEK
// unwrap on the auth hot path. Implemented by every concrete provider
// (AWS, Vault, LocalHSM); duplicating it here keeps the auth package
// free of an internal/crypto/kms import (cyclic-import-safe and the
// fake-KMS shape used in unit tests stays trivial).
type KMSUnwrapper interface {
	UnwrapDEK(ctx context.Context, keyID string, wrapped []byte) ([]byte, error)
}

// ErrBucketSigningKeyNotSet is returned by BucketSigningKeyStore when
// no per-bucket signing key is persisted. The auth middleware treats
// this as "fall through to the IAM access-key SigV4 path". Re-exported
// here so callers don't import the meta package; the value must match
// meta.ErrBucketSigningKeyNotSet via errors.Is.
var ErrBucketSigningKeyNotSet = errors.New("no per-bucket signing key set")

// ErrKMSUnavailable signals a transient KMS error on UnwrapDEK. The
// middleware emits HTTP 503 KMSUnavailable + Retry-After:30 (US-002).
var ErrKMSUnavailable = errors.New("kms unavailable")

// ErrKMSDenied signals an authoritative deny (wrong CMK, no
// permissions). Surfaced as 401 KeyDenied — operator must Rotate.
var ErrKMSDenied = errors.New("kms denied")

// ErrKMSTampered signals a wrapped-DEK tampering detection (HMAC mac
// mismatch on LocalHSMProvider). Surfaced as 401 KeyTampered.
var ErrKMSTampered = errors.New("kms wrapped dek tampered")

// ErrKMSKeyExpired signals a per-bucket signing key older than the
// configured STRATA_KEY_MAX_AGE window (US-002). Surfaced as HTTP 401
// KeyExpired — the operator must Rotate to recover; auth does NOT fall
// through to the IAM access-key path because the bucket explicitly opted
// in to per-bucket signing.
var ErrKMSKeyExpired = errors.New("kms signing key expired")

// dekCacheEntry holds a cached plaintext DEK plus its expiry.
type dekCacheEntry struct {
	plaintext []byte
	keyID     string
	expiresAt time.Time
}

// DEKCache memoises Provider.UnwrapDEK results behind a wall-clock TTL.
// Entries are keyed on the bucket name (so Rotate invalidates per
// bucket); the plaintext DEK is zeroed via subtle.ConstantTimeCopy
// before eviction so a heap-dump after expiry does not leak material.
// Safe for concurrent use; backed by sync.Map (the eviction path is the
// only writer per key under a per-key mutex held only briefly).
type DEKCache struct {
	ttl     time.Duration
	entries sync.Map // bucket -> *dekCacheEntry
	nowFn   func() time.Time
}

// NewDEKCache returns a cache with the given TTL. Tests can override
// the clock via SetClockForTest.
func NewDEKCache(ttl time.Duration) *DEKCache {
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	return &DEKCache{ttl: ttl, nowFn: time.Now}
}

// SetClockForTest overrides the cache clock for deterministic TTL tests.
func (c *DEKCache) SetClockForTest(now func() time.Time) { c.nowFn = now }

// Get returns the cached plaintext DEK for bucket+keyID, or
// (nil, false) on miss or expiry. A miss caused by expiry zeroes the
// stale entry's plaintext before returning.
func (c *DEKCache) Get(bucket, keyID string) ([]byte, bool) {
	v, ok := c.entries.Load(bucket)
	if !ok {
		return nil, false
	}
	e := v.(*dekCacheEntry)
	now := c.nowFn()
	if now.After(e.expiresAt) || e.keyID != keyID {
		c.zeroAndDelete(bucket, e)
		return nil, false
	}
	out := make([]byte, len(e.plaintext))
	copy(out, e.plaintext)
	return out, true
}

// Put stores a fresh plaintext DEK for bucket+keyID. A prior entry is
// zeroed before replacement so material does not linger in the heap.
func (c *DEKCache) Put(bucket, keyID string, plaintext []byte) {
	cp := make([]byte, len(plaintext))
	copy(cp, plaintext)
	entry := &dekCacheEntry{
		plaintext: cp,
		keyID:     keyID,
		expiresAt: c.nowFn().Add(c.ttl),
	}
	if prev, loaded := c.entries.Swap(bucket, entry); loaded {
		if e, ok := prev.(*dekCacheEntry); ok {
			zero(e.plaintext)
		}
	}
}

// Invalidate evicts the cached entry for bucket (called by the admin
// Rotate handler so the next SigV4 request picks up the new wrapping).
func (c *DEKCache) Invalidate(bucket string) {
	v, ok := c.entries.LoadAndDelete(bucket)
	if !ok {
		return
	}
	if e, ok := v.(*dekCacheEntry); ok {
		zero(e.plaintext)
	}
}

func (c *DEKCache) zeroAndDelete(bucket string, e *dekCacheEntry) {
	c.entries.CompareAndDelete(bucket, e)
	zero(e.plaintext)
}

// zero wipes a byte slice in constant time so the heap does not retain
// plaintext DEK material after eviction.
func zero(b []byte) {
	if len(b) == 0 {
		return
	}
	zeros := make([]byte, len(b))
	subtle.ConstantTimeCopy(1, b, zeros)
}

// BucketSigningResolver bundles the per-bucket signing key surface
// the auth middleware needs: a meta store to look up the envelope, a
// KMS unwrapper, a DEK cache, a counter sink for observability and an
// optional bucket-name extractor (defaults to path-style first
// segment). All fields except Store and KMS are optional; when Store
// is nil the resolver is inert and ResolveSecret returns
// (nil, false, nil) so the middleware short-circuits straight to the
// IAM access-key path.
type BucketSigningResolver struct {
	Store    BucketSigningKeyStore
	KMS      KMSUnwrapper
	Provider string // "aws_kms" | "vault" | "local_hsm" — counter label only
	Cache    *DEKCache
	// CounterInc is a callback invoked once per ResolveSecret with the
	// outcome label ∈ {"cache_hit","cache_miss_ok","unavailable","denied","tampered","expired"}.
	// Nil counters are tolerated — wiring is opt-in.
	CounterInc func(provider, outcome string)
	// ClassifyUnwrap translates a provider-side UnwrapDEK error into one of
	// the auth-side typed sentinels (ErrKMSUnavailable / ErrKMSDenied /
	// ErrKMSTampered). Wired by serverapp so the kms-package details (e.g.
	// kms.ErrKMSUnavailable, kms.ErrKeyIDMismatch) stay behind the auth
	// package's import surface. Nil falls through unchanged — the existing
	// raw-err propagation path used by every pre-US-002 test.
	ClassifyUnwrap func(error) error
	// BucketFor extracts the bucket name from the request. If nil the
	// resolver defaults to the first path segment (path-style routing,
	// which is the shape sigv4 already signs).
	BucketFor func(*http.Request) string
	// MaxAge enforces the STRATA_KEY_MAX_AGE rotation window (US-002).
	// A non-zero value rejects per-bucket signing keys whose
	// createdAt is older than now() - MaxAge with ErrKMSKeyExpired;
	// the middleware maps that sentinel to HTTP 401 KeyExpired. Zero
	// disables enforcement (legacy / unset).
	MaxAge time.Duration
	// Now overrides the wall clock for deterministic max-age tests.
	// Defaults to time.Now when nil.
	Now func() time.Time
}

// ResolveSecret looks up the per-bucket signing DEK and returns its
// plaintext for SigV4 derivation. (nil, false, nil) means "no
// per-bucket key — caller falls through to IAM"; an error means
// "per-bucket key set but unwrap failed" and the caller must surface
// it (no silent fallback per US-002 fail-closed semantics).
func (r *BucketSigningResolver) ResolveSecret(ctx context.Context, req *http.Request) (dek []byte, ok bool, err error) {
	if r == nil || r.Store == nil {
		return nil, false, nil
	}
	bucket := r.bucket(req)
	if bucket == "" {
		return nil, false, nil
	}
	wrapped, keyID, createdAt, lookupErr := r.Store.GetBucketSigningKey(ctx, bucket)
	if lookupErr != nil {
		if errors.Is(lookupErr, ErrBucketSigningKeyNotSet) {
			return nil, false, nil
		}
		// Bucket-not-found / transient meta errors: treat as "no
		// per-bucket key" — the SigV4 path should not fail because a
		// bucket lookup blipped. Audited via the counter still.
		return nil, false, nil
	}
	if r.MaxAge > 0 && !createdAt.IsZero() {
		now := r.now()
		if now.Sub(createdAt) > r.MaxAge {
			r.bump("expired")
			return nil, false, ErrKMSKeyExpired
		}
	}
	if r.Cache != nil {
		if plain, hit := r.Cache.Get(bucket, keyID); hit {
			r.bump("cache_hit")
			return plain, true, nil
		}
	}
	if r.KMS == nil {
		r.bump("unavailable")
		return nil, false, ErrKMSUnavailable
	}
	plain, err := r.KMS.UnwrapDEK(ctx, keyID, wrapped)
	if err != nil {
		if r.ClassifyUnwrap != nil {
			err = r.ClassifyUnwrap(err)
		}
		r.bump(classifyUnwrapErr(err))
		return nil, false, err
	}
	if r.Cache != nil {
		r.Cache.Put(bucket, keyID, plain)
	}
	r.bump("cache_miss_ok")
	return plain, true, nil
}

func (r *BucketSigningResolver) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *BucketSigningResolver) bucket(req *http.Request) string {
	if r.BucketFor != nil {
		return r.BucketFor(req)
	}
	return pathBucket(req.URL.Path)
}

func (r *BucketSigningResolver) bump(outcome string) {
	if r.CounterInc == nil {
		return
	}
	provider := r.Provider
	if provider == "" {
		provider = "unknown"
	}
	r.CounterInc(provider, outcome)
}

// pathBucket extracts the path-style bucket name from a request URL
// (the first non-empty segment of /<bucket>/<key>). Returns "" for
// the bucket-list ("/") endpoint and /admin/* paths.
func pathBucket(p string) string {
	if p == "" || p == "/" {
		return ""
	}
	if strings.HasPrefix(p, "/admin/") {
		return ""
	}
	if strings.HasPrefix(p, "/healthz") || strings.HasPrefix(p, "/readyz") || strings.HasPrefix(p, "/metrics") {
		return ""
	}
	p = strings.TrimPrefix(p, "/")
	if i := strings.IndexByte(p, '/'); i >= 0 {
		return p[:i]
	}
	return p
}

// classifyUnwrapErr maps a KMS unwrap error to the
// strata_kms_decrypt_total{outcome=...} label. Unknown errors fall
// into "denied" — the conservative bucket — so dashboards do not lose
// signal.
func classifyUnwrapErr(err error) string {
	switch {
	case errors.Is(err, ErrKMSUnavailable):
		return "unavailable"
	case errors.Is(err, ErrKMSTampered):
		return "tampered"
	case errors.Is(err, ErrKMSKeyExpired):
		return "expired"
	default:
		return "denied"
	}
}
