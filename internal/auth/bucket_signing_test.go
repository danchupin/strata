package auth

import (
	"context"
	"errors"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

type fakeKeyStore struct {
	wrapped []byte
	keyID   string
	created time.Time
	err     error
}

func (f *fakeKeyStore) GetBucketSigningKey(ctx context.Context, name string) ([]byte, string, time.Time, error) {
	if f.err != nil {
		return nil, "", time.Time{}, f.err
	}
	return f.wrapped, f.keyID, f.created, nil
}

type fakeKMS struct {
	plaintext []byte
	err       error
	calls     int32
}

func (f *fakeKMS) UnwrapDEK(ctx context.Context, keyID string, wrapped []byte) ([]byte, error) {
	atomic.AddInt32(&f.calls, 1)
	if f.err != nil {
		return nil, f.err
	}
	out := make([]byte, len(f.plaintext))
	copy(out, f.plaintext)
	return out, nil
}

func TestDEKCacheRoundTripAndExpiry(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	c := NewDEKCache(5 * time.Minute)
	c.SetClockForTest(clock)

	if _, ok := c.Get("b1", "k"); ok {
		t.Fatal("empty cache: expected miss")
	}

	dek := []byte{1, 2, 3, 4}
	c.Put("b1", "k", dek)
	got, ok := c.Get("b1", "k")
	if !ok {
		t.Fatal("after put: expected hit")
	}
	if string(got) != string(dek) {
		t.Fatalf("got %x want %x", got, dek)
	}

	// Different keyID busts entry.
	if _, ok := c.Get("b1", "k2"); ok {
		t.Fatal("mismatched keyID: expected miss")
	}
	// Re-insert (got busted by Get above due to keyID mismatch).
	c.Put("b1", "k", dek)

	// Advance past TTL.
	now = now.Add(6 * time.Minute)
	if _, ok := c.Get("b1", "k"); ok {
		t.Fatal("expired: expected miss")
	}
}

func TestDEKCacheInvalidate(t *testing.T) {
	c := NewDEKCache(time.Minute)
	c.Put("b1", "k", []byte{1, 2, 3})
	c.Invalidate("b1")
	if _, ok := c.Get("b1", "k"); ok {
		t.Fatal("after invalidate: expected miss")
	}
}

func TestBucketSigningResolverNoStore(t *testing.T) {
	var r *BucketSigningResolver
	req := httptest.NewRequest("GET", "/bkt/key", nil)
	dek, ok, err := r.ResolveSecret(context.Background(), req)
	if err != nil || ok || dek != nil {
		t.Fatalf("nil resolver: got dek=%x ok=%v err=%v", dek, ok, err)
	}
}

func TestBucketSigningResolverNotSet(t *testing.T) {
	store := &fakeKeyStore{err: ErrBucketSigningKeyNotSet}
	r := &BucketSigningResolver{Store: store}
	req := httptest.NewRequest("GET", "/bkt/key", nil)
	dek, ok, err := r.ResolveSecret(context.Background(), req)
	if err != nil || ok || dek != nil {
		t.Fatalf("not-set: got dek=%x ok=%v err=%v", dek, ok, err)
	}
}

func TestBucketSigningResolverCacheMissAndHit(t *testing.T) {
	store := &fakeKeyStore{wrapped: []byte{0xAB}, keyID: "kms-1"}
	kms := &fakeKMS{plaintext: []byte{0xCD, 0xEF}}
	cache := NewDEKCache(5 * time.Minute)
	var counterCalls []string
	r := &BucketSigningResolver{
		Store:    store,
		KMS:      kms,
		Provider: "aws_kms",
		Cache:    cache,
		CounterInc: func(_, outcome string) {
			counterCalls = append(counterCalls, outcome)
		},
	}
	req := httptest.NewRequest("GET", "/bkt/key", nil)

	// First call → cache miss → KMS unwrap.
	dek, ok, err := r.ResolveSecret(context.Background(), req)
	if err != nil || !ok {
		t.Fatalf("first call: ok=%v err=%v", ok, err)
	}
	if string(dek) != string(kms.plaintext) {
		t.Fatalf("dek mismatch: %x vs %x", dek, kms.plaintext)
	}
	if atomic.LoadInt32(&kms.calls) != 1 {
		t.Fatalf("KMS calls: got %d want 1", kms.calls)
	}

	// Second call → cache hit; KMS not called again.
	dek2, ok, err := r.ResolveSecret(context.Background(), req)
	if err != nil || !ok || string(dek2) != string(kms.plaintext) {
		t.Fatalf("second call: ok=%v err=%v dek=%x", ok, err, dek2)
	}
	if atomic.LoadInt32(&kms.calls) != 1 {
		t.Fatalf("cache should have absorbed second call; KMS calls=%d", kms.calls)
	}

	if len(counterCalls) != 2 || counterCalls[0] != "cache_miss_ok" || counterCalls[1] != "cache_hit" {
		t.Fatalf("counter outcomes: %v", counterCalls)
	}
}

func TestBucketSigningResolverKMSUnavailable(t *testing.T) {
	store := &fakeKeyStore{wrapped: []byte{0xAB}, keyID: "kms-1"}
	r := &BucketSigningResolver{Store: store, KMS: nil}
	req := httptest.NewRequest("GET", "/bkt/key", nil)
	dek, ok, err := r.ResolveSecret(context.Background(), req)
	if !errors.Is(err, ErrKMSUnavailable) || ok || dek != nil {
		t.Fatalf("expected ErrKMSUnavailable: got dek=%x ok=%v err=%v", dek, ok, err)
	}
}

func TestBucketSigningResolverKMSDenied(t *testing.T) {
	store := &fakeKeyStore{wrapped: []byte{0xAB}, keyID: "kms-1"}
	kms := &fakeKMS{err: errors.New("access denied")}
	var counterCalls []string
	r := &BucketSigningResolver{
		Store: store, KMS: kms, Provider: "aws_kms",
		CounterInc: func(_, outcome string) { counterCalls = append(counterCalls, outcome) },
	}
	req := httptest.NewRequest("GET", "/bkt/key", nil)
	_, ok, err := r.ResolveSecret(context.Background(), req)
	if err == nil || ok {
		t.Fatalf("expected error")
	}
	if len(counterCalls) != 1 || counterCalls[0] != "denied" {
		t.Fatalf("denied outcome missing: %v", counterCalls)
	}
}

func TestPathBucket(t *testing.T) {
	cases := map[string]string{
		"":              "",
		"/":             "",
		"/bkt":          "bkt",
		"/bkt/key":      "bkt",
		"/bkt/nested/x": "bkt",
		"/admin/v1/x":   "",
		"/healthz":      "",
		"/readyz":       "",
		"/metrics":      "",
	}
	for in, want := range cases {
		if got := pathBucket(in); got != want {
			t.Errorf("pathBucket(%q) = %q want %q", in, got, want)
		}
	}
}
