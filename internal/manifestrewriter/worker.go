// Package manifestrewriter walks every bucket's object versions and
// re-encodes any JSON-format manifest blob into protobuf. Idempotent:
// rows whose stored blob is already protobuf are skipped, so re-running
// after a crash or partial pass is safe. Drives cmd/strata-manifest-rewriter
// (US-049).
package manifestrewriter

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/meta"
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
}

// Worker performs a single-shot rewrite pass; not a daemon.
type Worker struct {
	cfg Config
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
		objScanned, objRewritten, skippedProto, err := w.rewriteBucket(ctx, b)
		if err != nil {
			return stats, fmt.Errorf("rewrite bucket %s: %w", b.Name, err)
		}
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
