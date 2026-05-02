package tikv

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	tikverr "github.com/tikv/client-go/v2/error"
	tikvkv "github.com/tikv/client-go/v2/kv"
	"github.com/tikv/client-go/v2/txnkv"
	"github.com/tikv/client-go/v2/txnkv/transaction"
)

// tikvBackend is the production kvBackend backed by a real TiKV cluster.
// One *txnkv.Client is shared by every Begin call.
type tikvBackend struct {
	cli *txnkv.Client
}

// newTiKVBackend dials PD and returns a backend ready for Begin. The caller
// is responsible for closing it.
func newTiKVBackend(pdEndpoints []string) (*tikvBackend, error) {
	if len(pdEndpoints) == 0 {
		return nil, errors.New("tikv: PDEndpoints must contain at least one address")
	}
	cli, err := txnkv.NewClient(pdEndpoints)
	if err != nil {
		return nil, fmt.Errorf("tikv: dial PD %v: %w", pdEndpoints, err)
	}
	return &tikvBackend{cli: cli}, nil
}

func (b *tikvBackend) Probe(ctx context.Context) error {
	if b == nil || b.cli == nil {
		return errors.New("tikv: backend not initialised")
	}
	_, err := b.cli.GetTimestamp(ctx)
	return err
}

func (b *tikvBackend) Close() error {
	if b == nil || b.cli == nil {
		return nil
	}
	return b.cli.Close()
}

func (b *tikvBackend) Begin(ctx context.Context, pessimistic bool) (kvTxn, error) {
	t, err := b.cli.Begin()
	if err != nil {
		return nil, err
	}
	if pessimistic {
		t.SetPessimistic(true)
	}
	return &tikvTxn{t: t, pessimistic: pessimistic}, nil
}

// tikvTxn adapts *transaction.KVTxn to the kvTxn interface.
type tikvTxn struct {
	t           *transaction.KVTxn
	pessimistic bool
}

func (x *tikvTxn) Get(ctx context.Context, key []byte) ([]byte, bool, error) {
	v, err := x.t.Get(ctx, key)
	if err == nil {
		return v, true, nil
	}
	if tikverr.IsErrNotFound(err) || errors.Is(err, tikverr.ErrNotExist) {
		return nil, false, nil
	}
	return nil, false, err
}

func (x *tikvTxn) Set(key, value []byte) error   { return x.t.Set(key, value) }
func (x *tikvTxn) Delete(key []byte) error       { return x.t.Delete(key) }
func (x *tikvTxn) Commit(ctx context.Context) error { return x.t.Commit(ctx) }
func (x *tikvTxn) Rollback() error               { return x.t.Rollback() }

func (x *tikvTxn) Scan(ctx context.Context, start, end []byte, limit int) ([]kvPair, error) {
	it, err := x.t.Iter(start, end)
	if err != nil {
		return nil, err
	}
	defer it.Close()
	var out []kvPair
	for it.Valid() {
		k := append([]byte(nil), it.Key()...)
		v := append([]byte(nil), it.Value()...)
		out = append(out, kvPair{Key: k, Value: v})
		if limit > 0 && len(out) >= limit {
			break
		}
		if err := it.Next(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (x *tikvTxn) LockKeys(ctx context.Context, keys ...[]byte) error {
	if !x.pessimistic {
		return errors.New("tikv: LockKeys called on optimistic transaction")
	}
	if len(keys) == 0 {
		return nil
	}
	lockCtx := tikvkv.NewLockCtx(x.t.StartTS(), int64(math.MaxInt64), time.Now())
	return x.t.LockKeys(ctx, lockCtx, keys...)
}
