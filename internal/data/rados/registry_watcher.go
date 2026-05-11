package rados

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"time"
)

// EnvRegistryInterval names the env knob that overrides the
// cluster-registry watcher's poll cadence.
const EnvRegistryInterval = "STRATA_CLUSTER_REGISTRY_INTERVAL"

const (
	defaultRegistryInterval = 30 * time.Second
	minRegistryInterval     = 5 * time.Second
	maxRegistryInterval     = 5 * time.Minute
)

// CatalogEntry is the watcher's view of one cluster_registry row. The
// internal/data/rados package cannot import internal/meta (cycle), so the
// watcher operates on this minimal projection of *meta.ClusterRegistryEntry.
// MetaCatalog (registry_meta_adapter.go) builds the projection for the
// production path.
type CatalogEntry struct {
	ID      string
	Backend string
	Spec    []byte
	Version int64
}

// CatalogReader is the registry source the watcher polls. *meta.Store
// implements it via MetaCatalog; tests substitute fakes.
type CatalogReader interface {
	ListClusters(ctx context.Context) ([]CatalogEntry, error)
}

// ReconcileSummary is the per-tick diff result emitted to the metrics sink.
type ReconcileSummary struct {
	Added   int
	Removed int
	Updated int
}

// ClusterReconciler applies the watcher's diff to the backend's in-memory
// cluster map. *rados.Backend implements it under the ceph build tag; a
// fakeReconciler stands in for unit tests.
type ClusterReconciler interface {
	ReconcileClusters(ctx context.Context, latest map[string]CatalogEntry) ReconcileSummary
}

// RegistryMetrics absorbs the prom counter ticks per reconciliation. The
// cmd-layer adapter (metrics.RADOSRegistryObserver) wires the strata_
// cluster_registry_changes_total counter; nil disables.
type RegistryMetrics interface {
	IncRegistryChange(op string, n int)
}

// RegistryWatcher polls the cluster_registry table every interval seconds
// and reconciles the result onto the backend's in-memory cluster map. Every
// gateway replica runs a watcher — the diff is idempotent so no leader
// election is needed.
type RegistryWatcher struct {
	reader   CatalogReader
	target   ClusterReconciler
	interval time.Duration
	logger   *slog.Logger
	metrics  RegistryMetrics

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

// NewRegistryWatcher constructs a watcher with the env-driven poll cadence
// (STRATA_CLUSTER_REGISTRY_INTERVAL, default 30s, clamped to [5s, 5m]).
// Either reader or target may be nil — Start() will error out in that case.
func NewRegistryWatcher(reader CatalogReader, target ClusterReconciler, logger *slog.Logger, metrics RegistryMetrics) *RegistryWatcher {
	return &RegistryWatcher{
		reader:   reader,
		target:   target,
		interval: registryIntervalFromEnv(),
		logger:   logger,
		metrics:  metrics,
	}
}

// Interval returns the resolved poll cadence — surfaced for tests.
func (w *RegistryWatcher) Interval() time.Duration { return w.interval }

// SyncOnce runs a single reconciliation pass. Returns immediately on the
// first error; partial state under b.mu is left as-is (next tick recomputes).
func (w *RegistryWatcher) SyncOnce(ctx context.Context) error {
	if w == nil {
		return nil
	}
	if w.reader == nil {
		return errors.New("rados: registry watcher has no reader")
	}
	if w.target == nil {
		return errors.New("rados: registry watcher has no reconciler")
	}
	rows, err := w.reader.ListClusters(ctx)
	if err != nil {
		return err
	}
	latest := make(map[string]CatalogEntry, len(rows))
	for _, r := range rows {
		latest[r.ID] = r
	}
	summary := w.target.ReconcileClusters(ctx, latest)
	if w.metrics != nil {
		if summary.Added > 0 {
			w.metrics.IncRegistryChange("add", summary.Added)
		}
		if summary.Removed > 0 {
			w.metrics.IncRegistryChange("remove", summary.Removed)
		}
		if summary.Updated > 0 {
			w.metrics.IncRegistryChange("update", summary.Updated)
		}
	}
	if w.logger != nil && (summary.Added|summary.Removed|summary.Updated) != 0 {
		w.logger.InfoContext(ctx, "rados cluster registry reconciled",
			"added", summary.Added, "removed", summary.Removed, "updated", summary.Updated)
	}
	return nil
}

// Start spawns the polling goroutine. Idempotent — a second call before
// Stop is a no-op. Cancellation of either the supplied parent ctx OR Stop()
// terminates the goroutine.
func (w *RegistryWatcher) Start(parent context.Context) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	w.cancel = cancel
	done := make(chan struct{})
	w.done = done
	go w.loop(ctx, done)
}

// Stop cancels the polling goroutine and waits for it to drain. Idempotent;
// safe to call from Backend.Close which is itself idempotent.
func (w *RegistryWatcher) Stop() {
	if w == nil {
		return
	}
	w.mu.Lock()
	cancel := w.cancel
	done := w.done
	w.cancel = nil
	w.done = nil
	w.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done
}

func (w *RegistryWatcher) loop(ctx context.Context, done chan struct{}) {
	defer close(done)
	t := time.NewTicker(w.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := w.SyncOnce(ctx); err != nil && w.logger != nil && ctx.Err() == nil {
				w.logger.WarnContext(ctx, "rados cluster registry watcher tick failed",
					"error", err.Error())
			}
		}
	}
}

func registryIntervalFromEnv() time.Duration {
	v := os.Getenv(EnvRegistryInterval)
	if v == "" {
		return defaultRegistryInterval
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return defaultRegistryInterval
	}
	if d < minRegistryInterval {
		return minRegistryInterval
	}
	if d > maxRegistryInterval {
		return maxRegistryInterval
	}
	return d
}
