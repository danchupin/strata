package memory

import (
	"context"
	"sync"
	"time"
)

type lockEntry struct {
	holder    string
	expiresAt time.Time
}

type Locker struct {
	mu    sync.Mutex
	locks map[string]*lockEntry
}

func NewLocker() *Locker {
	return &Locker{locks: make(map[string]*lockEntry)}
}

func (l *Locker) Acquire(ctx context.Context, name, holder string, ttl time.Duration) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if cur, ok := l.locks[name]; ok && cur.expiresAt.After(now) {
		return false, nil
	}
	l.locks[name] = &lockEntry{holder: holder, expiresAt: now.Add(ttl)}
	return true, nil
}

func (l *Locker) Renew(ctx context.Context, name, holder string, ttl time.Duration) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	cur, ok := l.locks[name]
	if !ok || cur.holder != holder {
		return false, nil
	}
	cur.expiresAt = time.Now().Add(ttl)
	return true, nil
}

func (l *Locker) Release(ctx context.Context, name, holder string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if cur, ok := l.locks[name]; ok && cur.holder == holder {
		delete(l.locks, name)
	}
	return nil
}
