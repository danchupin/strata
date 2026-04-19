package gc

import (
	"context"
	"log"
	"time"

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/metrics"
)

type Worker struct {
	Meta     meta.Store
	Data     data.Backend
	Region   string
	Interval time.Duration
	Grace    time.Duration
	Batch    int
	Logger   *log.Logger
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
		w.Logger = log.Default()
	}
	w.Logger.Printf("gc: starting (region=%s interval=%s grace=%s)", w.Region, w.Interval, w.Grace)

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
	for {
		entries, err := w.Meta.ListGCEntries(ctx, w.Region, before, w.Batch)
		if err != nil {
			w.Logger.Printf("gc: list: %v", err)
			return
		}
		if len(entries) == 0 {
			return
		}
		for _, e := range entries {
			manifest := &data.Manifest{Chunks: []data.ChunkRef{e.Chunk}}
			if err := w.Data.Delete(ctx, manifest); err != nil {
				w.Logger.Printf("gc: delete %s/%s: %v", e.Chunk.Pool, e.Chunk.OID, err)
				continue
			}
			if err := w.Meta.AckGCEntry(ctx, w.Region, e); err != nil {
				w.Logger.Printf("gc: ack %s/%s: %v", e.Chunk.Pool, e.Chunk.OID, err)
				continue
			}
			metrics.GCProcessed.Inc()
		}
		if len(entries) < w.Batch {
			return
		}
	}
}
