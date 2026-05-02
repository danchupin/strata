package tikv

import (
	"bytes"
	"context"
	"errors"
	"sort"
	"sync"
)

// memBackend is the in-process kvBackend used by unit tests. It is NOT a
// faithful TiKV emulator — concurrent semantics are simplified to keep the
// test code small. The contract suite that runs against the real TiKV
// (US-013) is the parity oracle.
//
// Semantics:
//
//   - All reads see the latest committed state plus the current txn's own
//     writes. There is no MVCC snapshot — sequential tests are the bar.
//   - Set/Delete are buffered until Commit. Rollback discards the buffer.
//   - LockKeys takes a per-key mutex per backend; the lock is released on
//     Commit/Rollback. Concurrent pessimistic txns serialize on contended
//     keys, which is enough to validate "create-if-not-exists conflict"
//     and "update-if-exists" flows in unit tests.
//   - Optimistic txns surface no write-conflicts (memBackend has none) —
//     prefer pessimistic in tests that care about CAS semantics.
type memBackend struct {
	mu   sync.Mutex
	data map[string][]byte
	// keyMu guards per-key pessimistic locks; entries are
	// the *sync.Mutex that LockKeys-holders are blocked on.
	keyMu map[string]*sync.Mutex
}

func newMemBackend() *memBackend {
	return &memBackend{
		data:  make(map[string][]byte),
		keyMu: make(map[string]*sync.Mutex),
	}
}

func (b *memBackend) Probe(ctx context.Context) error { return nil }
func (b *memBackend) Close() error                    { return nil }

func (b *memBackend) Begin(ctx context.Context, pessimistic bool) (kvTxn, error) {
	return &memTxn{
		backend:     b,
		pending:     make(map[string]memOp),
		pessimistic: pessimistic,
	}, nil
}

type memOp struct {
	delete bool
	value  []byte
}

type memTxn struct {
	backend     *memBackend
	pending     map[string]memOp
	heldLocks   []*sync.Mutex
	pessimistic bool
	done        bool
}

func (x *memTxn) Get(ctx context.Context, key []byte) ([]byte, bool, error) {
	if x.done {
		return nil, false, errors.New("tikv mem: txn closed")
	}
	if op, ok := x.pending[string(key)]; ok {
		if op.delete {
			return nil, false, nil
		}
		return append([]byte(nil), op.value...), true, nil
	}
	x.backend.mu.Lock()
	v, ok := x.backend.data[string(key)]
	x.backend.mu.Unlock()
	if !ok {
		return nil, false, nil
	}
	return append([]byte(nil), v...), true, nil
}

func (x *memTxn) Set(key, value []byte) error {
	if x.done {
		return errors.New("tikv mem: txn closed")
	}
	x.pending[string(key)] = memOp{value: append([]byte(nil), value...)}
	return nil
}

func (x *memTxn) Delete(key []byte) error {
	if x.done {
		return errors.New("tikv mem: txn closed")
	}
	x.pending[string(key)] = memOp{delete: true}
	return nil
}

func (x *memTxn) Scan(ctx context.Context, start, end []byte, limit int) ([]kvPair, error) {
	if x.done {
		return nil, errors.New("tikv mem: txn closed")
	}
	x.backend.mu.Lock()
	// Snapshot keys in [start, end) overlaid with txn-local pending writes.
	overlay := make(map[string][]byte, len(x.backend.data))
	for k, v := range x.backend.data {
		kb := []byte(k)
		if bytes.Compare(kb, start) < 0 {
			continue
		}
		if end != nil && bytes.Compare(kb, end) >= 0 {
			continue
		}
		overlay[k] = v
	}
	x.backend.mu.Unlock()
	for k, op := range x.pending {
		kb := []byte(k)
		if bytes.Compare(kb, start) < 0 {
			continue
		}
		if end != nil && bytes.Compare(kb, end) >= 0 {
			continue
		}
		if op.delete {
			delete(overlay, k)
			continue
		}
		overlay[k] = op.value
	}
	keys := make([]string, 0, len(overlay))
	for k := range overlay {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if limit > 0 && len(keys) > limit {
		keys = keys[:limit]
	}
	out := make([]kvPair, 0, len(keys))
	for _, k := range keys {
		v := overlay[k]
		out = append(out, kvPair{
			Key:   []byte(k),
			Value: append([]byte(nil), v...),
		})
	}
	return out, nil
}

func (x *memTxn) LockKeys(ctx context.Context, keys ...[]byte) error {
	if x.done {
		return errors.New("tikv mem: txn closed")
	}
	if !x.pessimistic {
		return errors.New("tikv mem: LockKeys called on optimistic transaction")
	}
	for _, k := range keys {
		x.backend.mu.Lock()
		mu, ok := x.backend.keyMu[string(k)]
		if !ok {
			mu = &sync.Mutex{}
			x.backend.keyMu[string(k)] = mu
		}
		x.backend.mu.Unlock()
		mu.Lock()
		x.heldLocks = append(x.heldLocks, mu)
	}
	return nil
}

func (x *memTxn) Commit(ctx context.Context) error {
	if x.done {
		return errors.New("tikv mem: txn closed")
	}
	x.backend.mu.Lock()
	for k, op := range x.pending {
		if op.delete {
			delete(x.backend.data, k)
			continue
		}
		x.backend.data[k] = append([]byte(nil), op.value...)
	}
	x.backend.mu.Unlock()
	x.releaseLocks()
	x.done = true
	return nil
}

func (x *memTxn) Rollback() error {
	if x.done {
		return nil
	}
	x.releaseLocks()
	x.done = true
	return nil
}

func (x *memTxn) releaseLocks() {
	for i := len(x.heldLocks) - 1; i >= 0; i-- {
		x.heldLocks[i].Unlock()
	}
	x.heldLocks = nil
}
