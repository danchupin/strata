// Audit-retention sweeper (US-009).
//
// TiKV has no native TTL — Cassandra's `USING TTL` lets the storage tier
// expunge expired rows without operator intervention; on TiKV we ride a
// per-row ExpiresAt stamp + a leader-elected goroutine that range-scans
// the audit prefix and deletes rows whose stamp has passed. Multiple
// gateway replicas all run the goroutine but only the lease holder
// actually deletes — the other replicas idle on AwaitAcquire.
package tikv

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/danchupin/strata/internal/leader"
	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/metrics"
)

// AuditSweepLeaderName is the leader-election lock key the sweeper takes.
// Distinct from "audit-sweeper-leader" so a hybrid Cassandra+TiKV
// deployment (smoke environments, migration windows) can run both
// without one stepping on the other's lease.
const AuditSweepLeaderName = "audit-sweeper-leader-tikv"

// AuditSweeperConfig wires the sweeper. Required: Locker (drives
// internal/leader.Session). Defaults: Interval=1h, Holder=leader-default,
// Logger=slog.Default(), Now=time.Now.
type AuditSweeperConfig struct {
	Store    *Store
	Locker   leader.Locker
	Interval time.Duration
	Holder   string
	Logger   *slog.Logger
	Now      func() time.Time
}

// AuditSweeper is a leader-elected goroutine that eager-deletes expired
// audit rows. Construct via NewAuditSweeper, drive via Run.
type AuditSweeper struct {
	cfg AuditSweeperConfig
}

// NewAuditSweeper validates cfg and returns a ready sweeper.
func NewAuditSweeper(cfg AuditSweeperConfig) (*AuditSweeper, error) {
	if cfg.Store == nil {
		return nil, errors.New("tikv: audit sweeper requires Store")
	}
	if cfg.Locker == nil {
		return nil, errors.New("tikv: audit sweeper requires Locker")
	}
	if cfg.Interval <= 0 {
		cfg.Interval = time.Hour
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Now().UTC() }
	}
	return &AuditSweeper{cfg: cfg}, nil
}

// Run blocks until ctx is cancelled. Acquires the leader lease,
// supervises it (lease loss → context cancel → re-acquire on next
// iteration), and ticks RunOnce every cfg.Interval while leader.
func (w *AuditSweeper) Run(ctx context.Context) error {
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		sess := &leader.Session{
			Locker: w.cfg.Locker,
			Name:   AuditSweepLeaderName,
			Holder: w.cfg.Holder,
			Logger: w.cfg.Logger,
		}
		if err := sess.AwaitAcquire(ctx); err != nil {
			return err
		}
		leaseCtx := sess.Supervise(ctx)
		w.cfg.Logger.InfoContext(ctx, "tikv audit sweeper running",
			"interval", w.cfg.Interval)
		// Run an immediate tick so a fresh leader does not wait a full
		// Interval before the first sweep.
		if _, err := w.RunOnce(leaseCtx); err != nil && !errors.Is(err, context.Canceled) {
			w.cfg.Logger.WarnContext(ctx, "tikv audit sweep failed", "error", err.Error())
		}
		ticker := time.NewTicker(w.cfg.Interval)
	tickLoop:
		for {
			select {
			case <-leaseCtx.Done():
				ticker.Stop()
				break tickLoop
			case <-ticker.C:
				if _, err := w.RunOnce(leaseCtx); err != nil && !errors.Is(err, context.Canceled) {
					w.cfg.Logger.WarnContext(ctx, "tikv audit sweep failed", "error", err.Error())
				}
			}
		}
		sess.Release(ctx)
		// Loop back: if ctx is alive, AwaitAcquire blocks until the lease
		// is regained; if not, the parent loop's ctx check returns.
	}
}

// RunOnce performs a single sweep pass over the entire audit prefix and
// returns the number of rows deleted. Exported so tests can drive it
// without leader election.
func (w *AuditSweeper) RunOnce(ctx context.Context) (int, error) {
	now := w.cfg.Now()
	keep := func(_ meta.AuditEvent, expiresAt time.Time) bool {
		// Keep rows with no expiry, or whose expiry is still in the future.
		return expiresAt.IsZero() || expiresAt.After(now)
	}
	deleted, err := w.cfg.Store.deleteAuditRange(
		ctx,
		[]byte(prefixAuditLog),
		prefixEnd([]byte(prefixAuditLog)),
		keep,
	)
	if deleted > 0 {
		metrics.MetaTikvAuditSweepDeleted.Add(float64(deleted))
		w.cfg.Logger.InfoContext(ctx, "tikv audit sweep deleted rows", "count", deleted)
	}
	return deleted, err
}
