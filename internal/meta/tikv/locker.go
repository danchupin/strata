// Cross-process leader-election locker on TiKV (US-011).
//
// Mirrors internal/meta/cassandra.Locker: a single row keyed on the
// lock name carries the current holder + an absolute expiry timestamp.
// Acquire CAS-flips an unheld-or-expired row to the new holder; Renew
// extends the expiry as long as we still hold; Release deletes the row
// when we still hold.
//
// Implementation notes:
//
//   - The underlying primitive is a TiKV pessimistic txn: LockKeys on
//     the lease row + Get + Set/Delete + Commit. The lock is a true
//     conflict-detection lease — but lock semantics alone do not survive
//     a process death (the txn is aborted at the TiKV side after the
//     pessimistic-lock timeout). The persistent ExpiresAt field is what
//     a NEW gateway uses to take over after the prior holder vanished:
//     when its Acquire call sees the row already present, it compares
//     the persisted ExpiresAt to now and steals if expired.
//
//   - All three methods use a fresh pessimistic txn — the lock is held
//     only for the duration of the read-modify-write, not for the full
//     lease lifetime. The persistent ExpiresAt + Holder fields drive
//     everyone else's "is this lease still alive?" check.
//
//   - This implementation is the production locker the gateway plugs in
//     when STRATA_META_BACKEND=tikv (production routing lands in US-015).
//     Sweeper unit tests still use the process-local dummyLocker in
//     locker_test.go because they don't want to spin a memBackend pair.
package tikv

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// Locker is the leader.Locker implementation backed by TiKV
// pessimistic txns. Construct via NewLocker(store) — it borrows the
// store's kvBackend.
type Locker struct {
	kv  kvBackend
	now func() time.Time
}

// NewLocker returns a Locker that takes leases against keys in the
// LeaderLockKey namespace using the supplied Store's kvBackend.
func NewLocker(s *Store) *Locker {
	if s == nil || s.kv == nil {
		return nil
	}
	return &Locker{kv: s.kv, now: func() time.Time { return time.Now().UTC() }}
}

// lockerLeaseRow is the persisted shape of one lock row.
type lockerLeaseRow struct {
	Holder    string    `json:"h"`
	ExpiresAt time.Time `json:"x"`
}

func (l *Locker) decode(raw []byte) (lockerLeaseRow, error) {
	var row lockerLeaseRow
	if err := json.Unmarshal(raw, &row); err != nil {
		return row, err
	}
	return row, nil
}

func (l *Locker) encode(holder string, expiresAt time.Time) ([]byte, error) {
	return json.Marshal(&lockerLeaseRow{Holder: holder, ExpiresAt: expiresAt})
}

// Acquire takes the lease for name on behalf of holder for ttl. Returns
// (true, nil) when the row was missing or the persisted ExpiresAt is in
// the past (lease was abandoned). Returns (false, nil) when an
// unexpired lease is held by a different holder. The lock-row update is
// CAS-style via a pessimistic txn so two concurrent Acquire calls cannot
// both observe an empty slot and both succeed.
func (l *Locker) Acquire(ctx context.Context, name, holder string, ttl time.Duration) (_ bool, err error) {
	if l == nil || l.kv == nil {
		return false, errors.New("tikv: locker not initialised")
	}
	key := LeaderLockKey(name)
	txn, err := l.kv.Begin(ctx, true)
	if err != nil {
		return false, err
	}
	defer rollbackOnError(txn, &err)
	if err = txn.LockKeys(ctx, key); err != nil {
		return false, err
	}
	raw, found, err := txn.Get(ctx, key)
	if err != nil {
		return false, err
	}
	now := l.now()
	if found {
		row, derr := l.decode(raw)
		if derr != nil {
			err = derr
			return false, err
		}
		if row.Holder != holder && row.ExpiresAt.After(now) {
			// Held by someone else and still alive — refuse.
			if rerr := txn.Rollback(); rerr != nil {
				return false, rerr
			}
			return false, nil
		}
	}
	payload, err := l.encode(holder, now.Add(ttl))
	if err != nil {
		return false, err
	}
	if err = txn.Set(key, payload); err != nil {
		return false, err
	}
	if err = txn.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// Renew refreshes the lease's ExpiresAt for the current holder. Returns
// (false, nil) when the lease has been stolen (different holder
// persisted) or has expired and the row is gone (a sibling can grab a
// missing slot). Mirrors Cassandra's `UPDATE ... USING TTL ... IF
// holder=?` shape.
func (l *Locker) Renew(ctx context.Context, name, holder string, ttl time.Duration) (_ bool, err error) {
	if l == nil || l.kv == nil {
		return false, errors.New("tikv: locker not initialised")
	}
	key := LeaderLockKey(name)
	txn, err := l.kv.Begin(ctx, true)
	if err != nil {
		return false, err
	}
	defer rollbackOnError(txn, &err)
	if err = txn.LockKeys(ctx, key); err != nil {
		return false, err
	}
	raw, found, err := txn.Get(ctx, key)
	if err != nil {
		return false, err
	}
	if !found {
		// Row vanished → lease lost. Mirror Cassandra's
		// gocql.ErrNotFound branch in Renew, which returns false
		// because the row TTL'd out between hold and renew.
		if rerr := txn.Rollback(); rerr != nil {
			return false, rerr
		}
		return false, nil
	}
	row, err := l.decode(raw)
	if err != nil {
		return false, err
	}
	if row.Holder != holder {
		if rerr := txn.Rollback(); rerr != nil {
			return false, rerr
		}
		return false, nil
	}
	payload, err := l.encode(holder, l.now().Add(ttl))
	if err != nil {
		return false, err
	}
	if err = txn.Set(key, payload); err != nil {
		return false, err
	}
	if err = txn.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// Release deletes the lease row when the persisted holder matches.
// Returns nil even when the row is missing or held by someone else
// (Cassandra's `DELETE ... IF holder=?` is similarly tolerant — the
// caller has already given up and stepping on a sibling's claim is
// worse than letting an abandoned row TTL out).
func (l *Locker) Release(ctx context.Context, name, holder string) (err error) {
	if l == nil || l.kv == nil {
		return errors.New("tikv: locker not initialised")
	}
	key := LeaderLockKey(name)
	txn, err := l.kv.Begin(ctx, true)
	if err != nil {
		return err
	}
	defer rollbackOnError(txn, &err)
	if err = txn.LockKeys(ctx, key); err != nil {
		return err
	}
	raw, found, err := txn.Get(ctx, key)
	if err != nil {
		return err
	}
	if !found {
		if rerr := txn.Rollback(); rerr != nil {
			return rerr
		}
		return nil
	}
	row, err := l.decode(raw)
	if err != nil {
		return err
	}
	if row.Holder != holder {
		if rerr := txn.Rollback(); rerr != nil {
			return rerr
		}
		return nil
	}
	if err = txn.Delete(key); err != nil {
		return err
	}
	return txn.Commit(ctx)
}
