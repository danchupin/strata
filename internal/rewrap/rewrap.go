// Package rewrap drives the SSE-S3 master-key rewrap pass. It walks every
// bucket's object versions and in-flight multipart uploads, unwraps each
// per-object DEK with the historical master key it was wrapped under, then
// re-wraps it with the active master key from the rotation list.
//
// Idempotency: rows whose stored SSEKeyID already equals the active key id
// are skipped. Re-running on already-current data is a no-op apart from the
// scan cost.
//
// Resumability: per-bucket progress is persisted via meta.Store.SetRewrapProgress
// after each bucket finishes. A subsequent run skips buckets whose recorded
// progress matches the current rotation's active key id and is marked complete.
package rewrap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/danchupin/strata/internal/crypto/master"
	ssecrypto "github.com/danchupin/strata/internal/crypto/sse"
	"github.com/danchupin/strata/internal/meta"
)

// Stats summarises a single Run.
type Stats struct {
	BucketsScanned    int
	BucketsSkipped    int
	ObjectsScanned    int
	ObjectsRewrapped  int
	UploadsScanned    int
	UploadsRewrapped  int
}

// Config wires a Worker.
type Config struct {
	Meta       meta.Store
	Provider   *master.RotationProvider
	Logger     *slog.Logger
	BatchLimit int           // page size for ListObjectVersions / ListMultipartUploads
	Now        func() time.Time
}

// Worker performs a single-shot rewrap pass; not a daemon.
type Worker struct {
	cfg Config
}

// New validates cfg and returns a worker.
func New(cfg Config) (*Worker, error) {
	if cfg.Meta == nil {
		return nil, errors.New("rewrap: meta store required")
	}
	if cfg.Provider == nil {
		return nil, errors.New("rewrap: rotation provider required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.BatchLimit <= 0 {
		cfg.BatchLimit = 500
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Worker{cfg: cfg}, nil
}

// Run walks every bucket and rewrap every encrypted object + in-flight
// multipart upload. Returns aggregated stats.
func (w *Worker) Run(ctx context.Context) (Stats, error) {
	var stats Stats
	buckets, err := w.cfg.Meta.ListBuckets(ctx, "")
	if err != nil {
		return stats, fmt.Errorf("list buckets: %w", err)
	}
	activeID := w.cfg.Provider.ActiveID()
	for _, b := range buckets {
		if ctx.Err() != nil {
			return stats, ctx.Err()
		}
		stats.BucketsScanned++

		prog, perr := w.cfg.Meta.GetRewrapProgress(ctx, b.ID)
		if perr != nil && !errors.Is(perr, meta.ErrNoRewrapProgress) {
			return stats, fmt.Errorf("get progress %s: %w", b.Name, perr)
		}
		if prog != nil && prog.Complete && prog.TargetID == activeID {
			stats.BucketsSkipped++
			w.cfg.Logger.Info("rewrap: skipping completed bucket", "bucket", b.Name, "target", activeID)
			continue
		}

		objCount, objRewrapped, err := w.rewrapBucketObjects(ctx, b)
		if err != nil {
			return stats, fmt.Errorf("rewrap bucket %s objects: %w", b.Name, err)
		}
		stats.ObjectsScanned += objCount
		stats.ObjectsRewrapped += objRewrapped

		upCount, upRewrapped, err := w.rewrapBucketUploads(ctx, b)
		if err != nil {
			return stats, fmt.Errorf("rewrap bucket %s uploads: %w", b.Name, err)
		}
		stats.UploadsScanned += upCount
		stats.UploadsRewrapped += upRewrapped

		if err := w.cfg.Meta.SetRewrapProgress(ctx, &meta.RewrapProgress{
			BucketID:  b.ID,
			TargetID:  activeID,
			Complete:  true,
			UpdatedAt: w.cfg.Now(),
		}); err != nil {
			return stats, fmt.Errorf("set progress %s: %w", b.Name, err)
		}
		w.cfg.Logger.Info("rewrap: bucket complete",
			"bucket", b.Name,
			"target", activeID,
			"objects_rewrapped", objRewrapped,
			"uploads_rewrapped", upRewrapped,
		)
	}
	return stats, nil
}

func (w *Worker) rewrapBucketObjects(ctx context.Context, b *meta.Bucket) (scanned, rewrapped int, err error) {
	activeID := w.cfg.Provider.ActiveID()
	activeKey, _, err := w.cfg.Provider.Resolve(ctx)
	if err != nil {
		return 0, 0, err
	}
	marker := ""
	for {
		if ctx.Err() != nil {
			return scanned, rewrapped, ctx.Err()
		}
		res, err := w.cfg.Meta.ListObjectVersions(ctx, b.ID, meta.ListOptions{
			Marker: marker,
			Limit:  w.cfg.BatchLimit,
		})
		if err != nil {
			return scanned, rewrapped, err
		}
		for _, o := range res.Versions {
			scanned++
			if len(o.SSEKey) == 0 {
				continue
			}
			if o.SSEKeyID == activeID {
				continue
			}
			oldKey, err := w.cfg.Provider.ResolveByID(ctx, o.SSEKeyID)
			if err != nil {
				return scanned, rewrapped, fmt.Errorf("object %q version %q: %w", o.Key, o.VersionID, err)
			}
			dek, err := ssecrypto.UnwrapDEK(oldKey, o.SSEKey)
			if err != nil {
				return scanned, rewrapped, fmt.Errorf("unwrap object %q version %q: %w", o.Key, o.VersionID, err)
			}
			newWrapped, err := ssecrypto.WrapDEK(activeKey, dek)
			if err != nil {
				return scanned, rewrapped, fmt.Errorf("rewrap object %q version %q: %w", o.Key, o.VersionID, err)
			}
			if err := w.cfg.Meta.UpdateObjectSSEWrap(ctx, b.ID, o.Key, o.VersionID, newWrapped, activeID); err != nil {
				return scanned, rewrapped, fmt.Errorf("persist object %q version %q: %w", o.Key, o.VersionID, err)
			}
			rewrapped++
		}
		if !res.Truncated {
			break
		}
		if res.NextKeyMarker == "" {
			break
		}
		marker = res.NextKeyMarker
	}
	return scanned, rewrapped, nil
}

func (w *Worker) rewrapBucketUploads(ctx context.Context, b *meta.Bucket) (scanned, rewrapped int, err error) {
	activeID := w.cfg.Provider.ActiveID()
	activeKey, _, err := w.cfg.Provider.Resolve(ctx)
	if err != nil {
		return 0, 0, err
	}
	uploads, err := w.cfg.Meta.ListMultipartUploads(ctx, b.ID, "", w.cfg.BatchLimit)
	if err != nil {
		return 0, 0, err
	}
	for _, u := range uploads {
		scanned++
		// ListMultipartUploads on the cassandra backend currently doesn't
		// project the SSE columns; refetch via GetMultipartUpload to get them.
		full, err := w.cfg.Meta.GetMultipartUpload(ctx, b.ID, u.UploadID)
		if err != nil {
			if errors.Is(err, meta.ErrMultipartNotFound) {
				continue
			}
			return scanned, rewrapped, err
		}
		if len(full.SSEKey) == 0 {
			continue
		}
		if full.SSEKeyID == activeID {
			continue
		}
		oldKey, err := w.cfg.Provider.ResolveByID(ctx, full.SSEKeyID)
		if err != nil {
			return scanned, rewrapped, fmt.Errorf("upload %q: %w", full.UploadID, err)
		}
		dek, err := ssecrypto.UnwrapDEK(oldKey, full.SSEKey)
		if err != nil {
			return scanned, rewrapped, fmt.Errorf("unwrap upload %q: %w", full.UploadID, err)
		}
		newWrapped, err := ssecrypto.WrapDEK(activeKey, dek)
		if err != nil {
			return scanned, rewrapped, fmt.Errorf("rewrap upload %q: %w", full.UploadID, err)
		}
		if err := w.cfg.Meta.UpdateMultipartUploadSSEWrap(ctx, b.ID, full.UploadID, newWrapped, activeID); err != nil {
			return scanned, rewrapped, fmt.Errorf("persist upload %q: %w", full.UploadID, err)
		}
		rewrapped++
	}
	return scanned, rewrapped, nil
}

