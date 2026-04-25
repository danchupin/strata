package auth

import (
	"context"
	"errors"
	"sync"
	"time"
)

// DefaultCacheTTL is the cache lifetime used when MultiStore is constructed
// without an explicit TTL. Matches the AC for US-005 (60s).
const DefaultCacheTTL = 60 * time.Second

// MultiStore composes multiple CredentialsStore backends and keeps a small
// in-memory cache. Stores are tried in order; the first hit wins. A negative
// result (ErrNoSuchCredential from every backend) is also cached so a flood of
// requests for an unknown key does not stampede every backend.
type MultiStore struct {
	stores []CredentialsStore
	ttl    time.Duration
	now    func() time.Time

	mu    sync.Mutex
	cache map[string]cacheEntry
}

type cacheEntry struct {
	cred      *Credential
	missing   bool
	expiresAt time.Time
}

func NewMultiStore(ttl time.Duration, stores ...CredentialsStore) *MultiStore {
	if ttl <= 0 {
		ttl = DefaultCacheTTL
	}
	return &MultiStore{
		stores: stores,
		ttl:    ttl,
		now:    time.Now,
		cache:  make(map[string]cacheEntry),
	}
}

func (m *MultiStore) Lookup(ctx context.Context, accessKey string) (*Credential, error) {
	if cred, missing, ok := m.fromCache(accessKey); ok {
		if missing {
			return nil, ErrNoSuchCredential
		}
		return cred, nil
	}

	var lastErr error
	for _, s := range m.stores {
		cred, err := s.Lookup(ctx, accessKey)
		if err == nil {
			// Temporary STS creds carry a SessionToken and have their own
			// expiry — never cache them, or a token revoked by the source
			// store would survive in the cache up to ttl.
			if cred.SessionToken == "" {
				m.put(accessKey, cred, false)
			}
			return cred, nil
		}
		if errors.Is(err, ErrNoSuchCredential) {
			continue
		}
		lastErr = err
	}
	if lastErr != nil {
		// Do not cache transient errors — caller should retry.
		return nil, lastErr
	}
	m.put(accessKey, nil, true)
	return nil, ErrNoSuchCredential
}

// Invalidate drops any cached entry for the given access key so the next
// Lookup re-reads the underlying stores. Callers should invoke this after
// rotating or deleting a key.
func (m *MultiStore) Invalidate(accessKey string) {
	m.mu.Lock()
	delete(m.cache, accessKey)
	m.mu.Unlock()
}

func (m *MultiStore) fromCache(accessKey string) (*Credential, bool, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.cache[accessKey]
	if !ok {
		return nil, false, false
	}
	if !e.expiresAt.After(m.now()) {
		delete(m.cache, accessKey)
		return nil, false, false
	}
	return e.cred, e.missing, true
}

func (m *MultiStore) put(accessKey string, cred *Credential, missing bool) {
	m.mu.Lock()
	m.cache[accessKey] = cacheEntry{
		cred:      cred,
		missing:   missing,
		expiresAt: m.now().Add(m.ttl),
	}
	m.mu.Unlock()
}
