package tikv

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"math"
	"time"

	tikvcfg "github.com/tikv/client-go/v2/config"
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
// is responsible for closing it. When tlsCfg.HasAny() returns true, the
// tikv-client-go global Security is updated before NewClient so the gRPC
// data path negotiates mTLS against TiKV + PD.
func newTiKVBackend(pdEndpoints []string, tlsCfg TLSConfig) (*tikvBackend, error) {
	if len(pdEndpoints) == 0 {
		return nil, errors.New("tikv: PDEndpoints must contain at least one address")
	}
	if tlsCfg.HasAny() {
		if err := applyTiKVSecurity(tlsCfg); err != nil {
			return nil, err
		}
	}
	cli, err := txnkv.NewClient(pdEndpoints)
	if err != nil {
		return nil, fmt.Errorf("tikv: dial PD %v: %w", pdEndpoints, err)
	}
	return &tikvBackend{cli: cli}, nil
}

// applyTiKVSecurity validates the supplied TLS materials and installs them
// onto tikv-client-go's global config.Security so subsequent NewClient calls
// pick them up. CAFile is required when TLS is enabled — the upstream
// Security.ToTLSConfig() short-circuits on empty ClusterSSLCA and falls back
// to plain-gRPC, which would silently defeat operator intent. CertFile +
// KeyFile must come paired. The cert pair is pre-loaded so PEM parse errors
// surface here rather than at first RPC.
func applyTiKVSecurity(tlsCfg TLSConfig) error {
	if tlsCfg.CAFile == "" {
		return errors.New("tikv tls: ca_file is required when any other tikv.tls.* knob is set (tikv-client-go Security.ToTLSConfig requires ClusterSSLCA)")
	}
	if (tlsCfg.CertFile == "") != (tlsCfg.KeyFile == "") {
		return errors.New("tikv tls: cert_file and key_file must both be set or both unset")
	}
	if tlsCfg.CertFile != "" {
		if _, err := tls.LoadX509KeyPair(tlsCfg.CertFile, tlsCfg.KeyFile); err != nil {
			return fmt.Errorf("tikv tls cert/key: %w", err)
		}
	}
	tikvcfg.UpdateGlobal(func(c *tikvcfg.Config) {
		c.Security = tikvcfg.NewSecurity(tlsCfg.CAFile, tlsCfg.CertFile, tlsCfg.KeyFile, nil)
	})
	return nil
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
