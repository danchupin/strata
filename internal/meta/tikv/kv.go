// kv is the small transactional surface internal/meta/tikv methods consume.
//
// The production implementation in kv_tikv.go wraps github.com/tikv/client-go
// txnkv.Client. The in-process implementation in kv_mem.go is for unit tests
// (US-013 lands the testcontainer-driven contract suite that exercises the
// real TiKV path). Methods here mirror the txnkv shape closely so the two
// implementations stay thin.
package tikv

import "context"

// kvBackend is the connection-level handle. Open*Backend constructors return
// an instance; Store calls Begin to start each transaction.
type kvBackend interface {
	// Begin starts a transaction. When pessimistic is true, subsequent
	// LockKeys calls acquire pessimistic locks so concurrent writers conflict
	// at lock-acquire time rather than at Commit. Optimistic transactions
	// still see write conflicts at Commit, but cannot LockKeys.
	Begin(ctx context.Context, pessimistic bool) (kvTxn, error)
	// Probe is the readiness probe consumed by the gateway /readyz endpoint.
	// Tikv impl asks PD for a fresh timestamp; mem impl is always ready.
	Probe(ctx context.Context) error
	Close() error
}

// kvTxn is the per-request transaction handle. Every method except Commit
// and Rollback can be called multiple times. Commit and Rollback are
// terminal — calling either invalidates the txn.
type kvTxn interface {
	// Get returns (value, true, nil) for an existing key, (nil, false, nil)
	// for a missing key, and (nil, false, err) for any other error.
	Get(ctx context.Context, key []byte) ([]byte, bool, error)
	Set(key, value []byte) error
	Delete(key []byte) error
	// Scan returns up to limit pairs whose key is in [start, end). Limit ≤ 0
	// means no limit. Pairs are returned in ascending key order.
	Scan(ctx context.Context, start, end []byte, limit int) ([]kvPair, error)
	// LockKeys takes pessimistic locks on the named keys for the lifetime of
	// the transaction. No-op for optimistic transactions; some backends may
	// return an error in that case.
	LockKeys(ctx context.Context, keys ...[]byte) error
	Commit(ctx context.Context) error
	Rollback() error
}

type kvPair struct {
	Key   []byte
	Value []byte
}
