package auth_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danchupin/strata/internal/auth"
)

// fakeStore is a minimal in-memory CredentialsStore used to drive MultiStore
// behaviour. It tracks how many times Lookup was called per key so cache
// behaviour can be asserted, and supports treating disabled rows as missing
// (matching cassandra.CredentialStore semantics).
type fakeStore struct {
	mu       sync.Mutex
	creds    map[string]*auth.Credential
	disabled map[string]bool
	err      error
	calls    int64
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		creds:    make(map[string]*auth.Credential),
		disabled: make(map[string]bool),
	}
}

func (s *fakeStore) put(c *auth.Credential, disabled bool) {
	s.mu.Lock()
	s.creds[c.AccessKey] = c
	s.disabled[c.AccessKey] = disabled
	s.mu.Unlock()
}

func (s *fakeStore) delete(key string) {
	s.mu.Lock()
	delete(s.creds, key)
	delete(s.disabled, key)
	s.mu.Unlock()
}

func (s *fakeStore) Lookup(_ context.Context, accessKey string) (*auth.Credential, error) {
	atomic.AddInt64(&s.calls, 1)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	c, ok := s.creds[accessKey]
	if !ok {
		return nil, auth.ErrNoSuchCredential
	}
	if s.disabled[accessKey] {
		return nil, auth.ErrNoSuchCredential
	}
	return c, nil
}

func TestMultiStore_LookupHit_StaticBeforeDynamic(t *testing.T) {
	static := newFakeStore()
	static.put(&auth.Credential{AccessKey: "AKIASTATIC", Secret: "s1", Owner: "alice"}, false)
	dyn := newFakeStore()
	dyn.put(&auth.Credential{AccessKey: "AKIASTATIC", Secret: "shadow", Owner: "shadow"}, false)

	m := auth.NewMultiStore(time.Minute, static, dyn)
	got, err := m.Lookup(context.Background(), "AKIASTATIC")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got.Secret != "s1" || got.Owner != "alice" {
		t.Fatalf("expected static cred to win, got %+v", got)
	}
	if atomic.LoadInt64(&dyn.calls) != 0 {
		t.Fatalf("dynamic store should not have been called when static hits")
	}
}

func TestMultiStore_LookupFallsThroughToDynamic(t *testing.T) {
	static := newFakeStore()
	dyn := newFakeStore()
	dyn.put(&auth.Credential{AccessKey: "AKIADYN", Secret: "d1", Owner: "bob"}, false)

	m := auth.NewMultiStore(time.Minute, static, dyn)
	got, err := m.Lookup(context.Background(), "AKIADYN")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got.Owner != "bob" {
		t.Fatalf("expected dynamic owner=bob, got %+v", got)
	}
}

func TestMultiStore_LookupMiss(t *testing.T) {
	static := newFakeStore()
	dyn := newFakeStore()
	m := auth.NewMultiStore(time.Minute, static, dyn)

	_, err := m.Lookup(context.Background(), "AKIAUNKNOWN")
	if !errors.Is(err, auth.ErrNoSuchCredential) {
		t.Fatalf("expected ErrNoSuchCredential, got %v", err)
	}
}

func TestMultiStore_DisabledLooksLikeMissing(t *testing.T) {
	dyn := newFakeStore()
	dyn.put(&auth.Credential{AccessKey: "AKIADIS", Secret: "x", Owner: "carol"}, true)
	m := auth.NewMultiStore(time.Minute, dyn)

	_, err := m.Lookup(context.Background(), "AKIADIS")
	if !errors.Is(err, auth.ErrNoSuchCredential) {
		t.Fatalf("expected disabled→ErrNoSuchCredential, got %v", err)
	}
}

func TestMultiStore_CacheHitAvoidsBackendCalls(t *testing.T) {
	dyn := newFakeStore()
	dyn.put(&auth.Credential{AccessKey: "AKIA1", Secret: "s", Owner: "o"}, false)
	m := auth.NewMultiStore(time.Minute, dyn)

	for range 5 {
		if _, err := m.Lookup(context.Background(), "AKIA1"); err != nil {
			t.Fatalf("lookup: %v", err)
		}
	}
	if got := atomic.LoadInt64(&dyn.calls); got != 1 {
		t.Fatalf("expected 1 backend call, got %d", got)
	}
}

func TestMultiStore_NegativeCache(t *testing.T) {
	dyn := newFakeStore()
	m := auth.NewMultiStore(time.Minute, dyn)

	for range 3 {
		if _, err := m.Lookup(context.Background(), "AKIA404"); !errors.Is(err, auth.ErrNoSuchCredential) {
			t.Fatalf("expected miss, got %v", err)
		}
	}
	if got := atomic.LoadInt64(&dyn.calls); got != 1 {
		t.Fatalf("expected 1 backend call (negatives cached), got %d", got)
	}
}

func TestMultiStore_KeyRotationViaInvalidate(t *testing.T) {
	dyn := newFakeStore()
	dyn.put(&auth.Credential{AccessKey: "AKIA1", Secret: "old", Owner: "o"}, false)
	m := auth.NewMultiStore(time.Minute, dyn)

	c, err := m.Lookup(context.Background(), "AKIA1")
	if err != nil || c.Secret != "old" {
		t.Fatalf("first lookup: %v %+v", err, c)
	}

	// rotate the secret in the backend; cache still holds the old value.
	dyn.put(&auth.Credential{AccessKey: "AKIA1", Secret: "new", Owner: "o"}, false)
	c, _ = m.Lookup(context.Background(), "AKIA1")
	if c.Secret != "old" {
		t.Fatalf("cache should still hold old secret, got %q", c.Secret)
	}

	m.Invalidate("AKIA1")
	c, err = m.Lookup(context.Background(), "AKIA1")
	if err != nil {
		t.Fatalf("post-invalidate lookup: %v", err)
	}
	if c.Secret != "new" {
		t.Fatalf("post-invalidate secret should be new, got %q", c.Secret)
	}
}

func TestMultiStore_DeletedKeyReturnsErrNoSuchCredential(t *testing.T) {
	dyn := newFakeStore()
	dyn.put(&auth.Credential{AccessKey: "AKIA1", Secret: "s", Owner: "o"}, false)
	m := auth.NewMultiStore(time.Minute, dyn)

	if _, err := m.Lookup(context.Background(), "AKIA1"); err != nil {
		t.Fatalf("first lookup: %v", err)
	}
	dyn.delete("AKIA1")
	m.Invalidate("AKIA1")
	if _, err := m.Lookup(context.Background(), "AKIA1"); !errors.Is(err, auth.ErrNoSuchCredential) {
		t.Fatalf("expected ErrNoSuchCredential after delete, got %v", err)
	}
}

func TestMultiStore_TTLExpires(t *testing.T) {
	dyn := newFakeStore()
	dyn.put(&auth.Credential{AccessKey: "AKIA1", Secret: "s", Owner: "o"}, false)
	m := auth.NewMultiStore(20*time.Millisecond, dyn)

	if _, err := m.Lookup(context.Background(), "AKIA1"); err != nil {
		t.Fatalf("first lookup: %v", err)
	}
	time.Sleep(40 * time.Millisecond)
	if _, err := m.Lookup(context.Background(), "AKIA1"); err != nil {
		t.Fatalf("second lookup: %v", err)
	}
	if got := atomic.LoadInt64(&dyn.calls); got != 2 {
		t.Fatalf("expected 2 backend calls after TTL expiry, got %d", got)
	}
}

func TestMultiStore_TransientErrorNotCached(t *testing.T) {
	dyn := newFakeStore()
	dyn.err = errors.New("boom")
	m := auth.NewMultiStore(time.Minute, dyn)

	if _, err := m.Lookup(context.Background(), "AKIA1"); err == nil {
		t.Fatalf("expected error")
	}
	dyn.mu.Lock()
	dyn.err = nil
	dyn.creds["AKIA1"] = &auth.Credential{AccessKey: "AKIA1", Secret: "s", Owner: "o"}
	dyn.disabled["AKIA1"] = false
	dyn.mu.Unlock()

	c, err := m.Lookup(context.Background(), "AKIA1")
	if err != nil {
		t.Fatalf("expected recovery, got %v", err)
	}
	if c.Owner != "o" {
		t.Fatalf("unexpected cred: %+v", c)
	}
}
