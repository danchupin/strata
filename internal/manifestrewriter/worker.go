// Package manifestrewriter walks every bucket's object versions and
// re-encodes any JSON-format manifest blob into protobuf. Idempotent:
// rows whose stored blob is already protobuf are skipped, so re-running
// after a crash or partial pass is safe. Drives the manifest-rewriter
// worker registered in cmd/strata/workers (US-049 / US-012).
package manifestrewriter

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/meta"
	strataotel "github.com/danchupin/strata/internal/otel"
)

// Stats summarises a single Run.
type Stats struct {
	BucketsScanned     int
	ObjectsScanned     int
	ObjectsRewritten   int
	ObjectsSkippedProto int
}

// Config wires a Worker.
type Config struct {
	Meta       meta.Store
	Logger     *slog.Logger
	BatchLimit int // page size for ListObjectVersions
	DryRun     bool
	// Tracer emits per-iteration parent spans (`worker.manifest-rewriter.tick`)
	// plus `manifest_rewriter.rewrite_bucket` sub-op children. Nil falls back
	// to a process-shared no-op tracer.
	Tracer trace.Tracer
}

// Worker performs a single-shot rewrite pass; not a daemon.
type Worker struct {
	cfg Config

	iterErrMu sync.Mutex
	iterErr   error
}

func (w *Worker) tracerOrNoop() trace.Tracer {
	if w.cfg.Tracer == nil {
		return strataotel.NoopTracer()
	}
	return w.cfg.Tracer
}

func (w *Worker) recordIterErr(err error) {
	if err == nil {
		return
	}
	w.iterErrMu.Lock()
	if w.iterErr == nil {
		w.iterErr = err
	}
	w.iterErrMu.Unlock()
}

func (w *Worker) takeIterErr() error {
	w.iterErrMu.Lock()
	defer w.iterErrMu.Unlock()
	err := w.iterErr
	w.iterErr = nil
	return err
}

// New validates cfg and returns a Worker.
func New(cfg Config) (*Worker, error) {
	if cfg.Meta == nil {
		return nil, errors.New("manifestrewriter: meta store required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.BatchLimit <= 0 {
		cfg.BatchLimit = 500
	}
	return &Worker{cfg: cfg}, nil
}

// Run walks every bucket and converts any JSON manifest to protobuf.
// Returns aggregated stats. Honours ctx cancellation between rows.
func (w *Worker) Run(ctx context.Context) (Stats, error) {
	iterCtx, span := strataotel.StartIteration(ctx, w.tracerOrNoop(), "manifest-rewriter")
	stats, err := w.runOnce(iterCtx)
	if err == nil {
		err = w.takeIterErr()
	} else {
		_ = w.takeIterErr()
	}
	strataotel.EndIteration(span, err)
	return stats, err
}

func (w *Worker) runOnce(ctx context.Context) (Stats, error) {
	var stats Stats
	buckets, err := w.cfg.Meta.ListBuckets(ctx, "")
	if err != nil {
		return stats, fmt.Errorf("list buckets: %w", err)
	}
	for _, b := range buckets {
		if ctx.Err() != nil {
			return stats, ctx.Err()
		}
		stats.BucketsScanned++
		bucketCtx, bucketSpan := w.tracerOrNoop().Start(ctx, "manifest_rewriter.rewrite_bucket",
			trace.WithSpanKind(trace.SpanKindInternal),
			trace.WithAttributes(
				strataotel.AttrComponentWorker,
				attribute.String(strataotel.WorkerKey, "manifest-rewriter"),
				attribute.String("strata.manifest_rewriter.bucket", b.Name),
				attribute.String("strata.manifest_rewriter.bucket_id", b.ID.String()),
			),
		)
		objScanned, objRewritten, skippedProto, rerr := w.rewriteBucket(bucketCtx, b)
		bucketSpan.SetAttributes(
			attribute.Int("strata.manifest_rewriter.scanned", objScanned),
			attribute.Int("strata.manifest_rewriter.rewritten", objRewritten),
			attribute.Int("strata.manifest_rewriter.already_proto", skippedProto),
		)
		if rerr != nil {
			bucketSpan.RecordError(rerr)
			bucketSpan.SetStatus(codes.Error, rerr.Error())
			w.recordIterErr(rerr)
			bucketSpan.End()
			return stats, fmt.Errorf("rewrite bucket %s: %w", b.Name, rerr)
		}
		bucketSpan.End()
		stats.ObjectsScanned += objScanned
		stats.ObjectsRewritten += objRewritten
		stats.ObjectsSkippedProto += skippedProto
		w.cfg.Logger.Info("manifestrewriter: bucket complete",
			"bucket", b.Name,
			"scanned", objScanned,
			"rewritten", objRewritten,
			"already_proto", skippedProto,
			"dry_run", w.cfg.DryRun,
		)
	}
	return stats, nil
}

func (w *Worker) rewriteBucket(ctx context.Context, b *meta.Bucket) (scanned, rewritten, skippedProto int, err error) {
	marker := ""
	for {
		if ctx.Err() != nil {
			return scanned, rewritten, skippedProto, ctx.Err()
		}
		res, lerr := w.cfg.Meta.ListObjectVersions(ctx, b.ID, meta.ListOptions{
			Marker: marker,
			Limit:  w.cfg.BatchLimit,
		})
		if lerr != nil {
			return scanned, rewritten, skippedProto, lerr
		}
		for _, o := range res.Versions {
			scanned++
			if o.IsDeleteMarker {
				continue
			}
			raw, gerr := w.cfg.Meta.GetObjectManifestRaw(ctx, b.ID, o.Key, o.VersionID)
			if gerr != nil {
				if errors.Is(gerr, meta.ErrObjectNotFound) {
					// Race with delete — skip.
					continue
				}
				return scanned, rewritten, skippedProto, fmt.Errorf("get raw %q v=%q: %w", o.Key, o.VersionID, gerr)
			}
			if len(raw) == 0 {
				continue
			}
			if !data.IsManifestJSON(raw) {
				skippedProto++
				continue
			}
			m, derr := data.DecodeManifest(raw)
			if derr != nil {
				return scanned, rewritten, skippedProto, fmt.Errorf("decode %q v=%q: %w", o.Key, o.VersionID, derr)
			}
			protoBlob, eerr := data.EncodeManifestProto(m)
			if eerr != nil {
				return scanned, rewritten, skippedProto, fmt.Errorf("encode proto %q v=%q: %w", o.Key, o.VersionID, eerr)
			}
			if w.cfg.DryRun {
				rewritten++
				continue
			}
			if uerr := w.cfg.Meta.UpdateObjectManifestRaw(ctx, b.ID, o.Key, o.VersionID, protoBlob); uerr != nil {
				if errors.Is(uerr, meta.ErrObjectNotFound) {
					continue
				}
				return scanned, rewritten, skippedProto, fmt.Errorf("persist %q v=%q: %w", o.Key, o.VersionID, uerr)
			}
			rewritten++
		}
		if !res.Truncated || res.NextKeyMarker == "" {
			break
		}
		marker = res.NextKeyMarker
	}
	return scanned, rewritten, skippedProto, nil
}
