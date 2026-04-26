package gc

import (
	"context"
	"log/slog"
	"time"

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/metrics"
)

// Metrics is the narrow observer the worker uses to publish queue depth.
// Cmd-layer plugs metrics.GCObserver{}.
type Metrics interface {
	SetQueueDepth(region string, depth int)
}

type Worker struct {
	Meta     meta.Store
	Data     data.Backend
	Region   string
	Interval time.Duration
	Grace    time.Duration
	Batch    int
	Logger   *slog.Logger
	Metrics  Metrics
}

func (w *Worker) Run(ctx context.Context) error {
	if w.Interval == 0 {
		w.Interval = 30 * time.Second
	}
	if w.Grace < 0 {
		w.Grace = 0
	}
	if w.Batch == 0 {
		w.Batch = 500
	}
	if w.Logger == nil {
		w.Logger = slog.Default()
	}
	w.Logger.InfoContext(ctx, "gc: starting", "region", w.Region, "interval", w.Interval.String(), "grace", w.Grace.String())

	ticker := time.NewTicker(w.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			w.drain(ctx)
		}
	}
}

func (w *Worker) drain(ctx context.Context) {
	before := time.Now().Add(-w.Grace)
	first := true
	for {
		entries, err := w.Meta.ListGCEntries(ctx, w.Region, before, w.Batch)
		if err != nil {
			w.Logger.WarnContext(ctx, "gc list", "error", err.Error())
			return
		}
		if first && w.Metrics != nil {
			w.Metrics.SetQueueDepth(w.Region, len(entries))
		}
		first = false
		if len(entries) == 0 {
			return
		}
		for _, e := range entries {
			manifest := &data.Manifest{Chunks: []data.ChunkRef{e.Chunk}}
			if err := w.Data.Delete(ctx, manifest); err != nil {
				w.Logger.WarnContext(ctx, "gc delete", "pool", e.Chunk.Pool, "oid", e.Chunk.OID, "error", err.Error())
				continue
			}
			if err := w.Meta.AckGCEntry(ctx, w.Region, e); err != nil {
				w.Logger.WarnContext(ctx, "gc ack", "pool", e.Chunk.Pool, "oid", e.Chunk.OID, "error", err.Error())
				continue
			}
			metrics.GCProcessed.Inc()
		}
		if len(entries) < w.Batch {
			return
		}
	}
}
