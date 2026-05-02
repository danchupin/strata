package tikv

import (
	"context"
	"sync"
	"time"
)

// dummyLocker is a process-local leader.Locker used by the audit-sweeper
// unit tests. The real cross-process locker on TiKV (US-011) takes a
// pessimistic-txn lease against LeaderLockKey; for sweeper unit tests we
// only need leader.Session.AwaitAcquire to return promptly so we can
// drive RunOnce.
type dummyLocker struct {
	mu      sync.Mutex
	holders map[string]string
	expiry  map[string]time.Time
}

func newDummyLocker() *dummyLocker {
	return &dummyLocker{
		holders: map[string]string{},
		expiry:  map[string]time.Time{},
	}
}

func (l *dummyLocker) Acquire(ctx context.Context, name, holder string, ttl time.Duration) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if h, ok := l.holders[name]; ok && h != holder {
		if now.Before(l.expiry[name]) {
			return false, nil
		}
	}
	l.holders[name] = holder
	l.expiry[name] = now.Add(ttl)
	return true, nil
}

func (l *dummyLocker) Renew(ctx context.Context, name, holder string, ttl time.Duration) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.holders[name] != holder {
		return false, nil
	}
	l.expiry[name] = time.Now().Add(ttl)
	return true, nil
}

func (l *dummyLocker) Release(ctx context.Context, name, holder string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.holders[name] == holder {
		delete(l.holders, name)
		delete(l.expiry, name)
	}
	return nil
}
