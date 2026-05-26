package tikv

import (
	"context"
	"errors"

	tikverr "github.com/tikv/client-go/v2/error"
)

// opCtxKey is the unexported type for the per-Store-method op label stashed
// in ctx by Observer.Start. The pessimistic-txn outcome counter pulls it
// back out at Begin time so the metric label matches the Store method
// without per-callsite plumbing (US-001 cycle B prod-observability).
type opCtxKey struct{}

func withOp(ctx context.Context, op string) context.Context {
	if op == "" {
		return ctx
	}
	return context.WithValue(ctx, opCtxKey{}, op)
}

func opFromCtx(ctx context.Context) string {
	if v, ok := ctx.Value(opCtxKey{}).(string); ok && v != "" {
		return v
	}
	return "unknown"
}

// beginPessimistic opens a pessimistic kvTxn and wraps it so Commit /
// Rollback bump the strata_tikv_pessimistic_txn_total counter once per
// terminal outcome. The op label is sourced from ctx (stashed by
// Observer.Start at every Store method entry).
//
// Call sites replace the historical `s.kv.Begin(ctx, true)` direct call.
// Optimistic txns (used by snapshot reads) stay on the direct path —
// they never LockKeys, never conflict, and are out of scope for this
// counter.
func (s *Store) beginPessimistic(ctx context.Context) (kvTxn, error) {
	txn, err := s.kv.Begin(ctx, true)
	if err != nil {
		return nil, err
	}
	if s.metrics == nil {
		return txn, nil
	}
	return &metricsTxn{inner: txn, metrics: s.metrics, op: opFromCtx(ctx)}, nil
}

// metricsTxn wraps kvTxn and bumps the per-outcome counter at Commit /
// Rollback. Idempotent — only the first terminal call bumps. Concurrent
// terminal calls are not expected (txnkv.KVTxn already serialises) but
// the `done` guard keeps the metric honest if a caller doublecounts.
type metricsTxn struct {
	inner   kvTxn
	metrics Metrics
	op      string
	done    bool
}

func (m *metricsTxn) Get(ctx context.Context, key []byte) ([]byte, bool, error) {
	return m.inner.Get(ctx, key)
}

func (m *metricsTxn) Set(key, value []byte) error { return m.inner.Set(key, value) }
func (m *metricsTxn) Delete(key []byte) error     { return m.inner.Delete(key) }

func (m *metricsTxn) Scan(ctx context.Context, start, end []byte, limit int) ([]kvPair, error) {
	return m.inner.Scan(ctx, start, end, limit)
}

func (m *metricsTxn) LockKeys(ctx context.Context, keys ...[]byte) error {
	return m.inner.LockKeys(ctx, keys...)
}

func (m *metricsTxn) Commit(ctx context.Context) error {
	err := m.inner.Commit(ctx)
	if m.done {
		return err
	}
	m.done = true
	switch {
	case err == nil:
		m.metrics.IncPessimisticTxn(m.op, "commit")
	case isPessimisticConflict(err):
		m.metrics.IncPessimisticTxn(m.op, "conflict")
	default:
		m.metrics.IncPessimisticTxn(m.op, "rollback")
	}
	return err
}

func (m *metricsTxn) Rollback() error {
	if !m.done {
		m.done = true
		m.metrics.IncPessimisticTxn(m.op, "rollback")
	}
	return m.inner.Rollback()
}

// isPessimisticConflict returns true when err comes from TiKV's
// pessimistic-conflict surface (write-conflict, deadlock, lock timeout).
// Other Commit failures (e.g. network, PD down) flow into the "rollback"
// outcome bucket so operators get a clean signal split. ErrWriteConflict
// + ErrDeadlock are struct-shaped errors in tikv-client-go; the sentinel
// matchers below catch them via errors.As. ErrLockAcquireFailAndNoWaitSet
// + ErrLockWaitTimeout are plain errors.New sentinels (errors.Is works).
func isPessimisticConflict(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, tikverr.ErrLockAcquireFailAndNoWaitSet) ||
		errors.Is(err, tikverr.ErrLockWaitTimeout) {
		return true
	}
	var wc *tikverr.ErrWriteConflict
	if errors.As(err, &wc) {
		return true
	}
	var dl *tikverr.ErrDeadlock
	if errors.As(err, &dl) {
		return true
	}
	return false
}
