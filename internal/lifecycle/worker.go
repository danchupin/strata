package lifecycle

import (
	"context"
	"errors"
	"log/slog"
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
	AgeUnit  time.Duration
	Logger   *slog.Logger
}

func (w *Worker) Run(ctx context.Context) error {
	if w.Interval == 0 {
		w.Interval = 60 * time.Second
	}
	if w.AgeUnit == 0 {
		w.AgeUnit = 24 * time.Hour
	}
	if w.Logger == nil {
		w.Logger = slog.Default()
	}
	w.Logger.InfoContext(ctx, "lifecycle: starting", "interval", w.Interval.String(), "age_unit", w.AgeUnit.String())

	ticker := time.NewTicker(w.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := w.runOnce(ctx); err != nil {
				w.Logger.WarnContext(ctx, "lifecycle tick failed", "error", err.Error())
			}
		}
	}
}

func (w *Worker) RunOnce(ctx context.Context) error { return w.runOnce(ctx) }

func (w *Worker) runOnce(ctx context.Context) error {
	buckets, err := w.Meta.ListBuckets(ctx, "")
	if err != nil {
		return err
	}
	for _, b := range buckets {
		blob, err := w.Meta.GetBucketLifecycle(ctx, b.ID)
		if err != nil {
			if errors.Is(err, meta.ErrNoSuchLifecycle) {
				continue
			}
			w.Logger.WarnContext(ctx, "lifecycle get rules", "bucket", b.Name, "error", err.Error())
			continue
		}
		cfg, err := Parse(blob)
		if err != nil {
			w.Logger.WarnContext(ctx, "lifecycle parse", "bucket", b.Name, "error", err.Error())
			continue
		}
		for i := range cfg.Rules {
			rule := &cfg.Rules[i]
			if !rule.IsEnabled() {
				continue
			}
			if err := w.applyRule(ctx, b, rule); err != nil {
				w.Logger.WarnContext(ctx, "lifecycle apply rule", "bucket", b.Name, "rule", rule.ID, "error", err.Error())
			}
		}
	}
	return nil
}

func (w *Worker) applyRule(ctx context.Context, b *meta.Bucket, rule *Rule) error {
	if rule.AbortIncompleteMultipartUpload != nil && rule.AbortIncompleteMultipartUpload.DaysAfterInitiation > 0 {
		w.abortStaleUploads(ctx, b, rule)
	}
	if rule.HasNoncurrentActions() && meta.IsVersioningActive(b.Versioning) {
		if err := w.applyNoncurrentActions(ctx, b, rule); err != nil {
			return err
		}
	}
	if rule.Transition == nil && rule.Expiration == nil {
		return nil
	}
	opts := meta.ListOptions{Prefix: rule.PrefixMatch(), Limit: 1000}
	for {
		res, err := w.Meta.ListObjects(ctx, b.ID, opts)
		if err != nil {
			return err
		}
		for _, o := range res.Objects {
			w.evaluate(ctx, b, rule, o)
		}
		if !res.Truncated {
			return nil
		}
		opts.Marker = res.NextMarker
	}
}

func (w *Worker) applyNoncurrentActions(ctx context.Context, b *meta.Bucket, rule *Rule) error {
	opts := meta.ListOptions{Prefix: rule.PrefixMatch(), Limit: 1000}
	for {
		res, err := w.Meta.ListObjectVersions(ctx, b.ID, opts)
		if err != nil {
			return err
		}
		var prevKey string
		var prevMtime time.Time
		for _, v := range res.Versions {
			if v.Key != prevKey {
				prevKey = v.Key
				prevMtime = v.Mtime
				continue
			}
			noncurrentSince := prevMtime
			prevMtime = v.Mtime
			age := time.Since(noncurrentSince)
			w.evaluateNoncurrent(ctx, b, rule, v, age)
		}
		if !res.Truncated {
			return nil
		}
		opts.Marker = res.NextKeyMarker
	}
}

func (w *Worker) evaluateNoncurrent(ctx context.Context, b *meta.Bucket, rule *Rule, v *meta.Object, age time.Duration) {
	if rule.NoncurrentVersionExpiration != nil && rule.NoncurrentVersionExpiration.NoncurrentDays > 0 {
		threshold := time.Duration(rule.NoncurrentVersionExpiration.NoncurrentDays) * w.AgeUnit
		if age >= threshold {
			w.expireNoncurrent(ctx, b, v, rule.ID)
			return
		}
	}
	if rule.NoncurrentVersionTransition != nil && rule.NoncurrentVersionTransition.NoncurrentDays > 0 && rule.NoncurrentVersionTransition.StorageClass != "" {
		if v.StorageClass == rule.NoncurrentVersionTransition.StorageClass || v.IsDeleteMarker {
			return
		}
		threshold := time.Duration(rule.NoncurrentVersionTransition.NoncurrentDays) * w.AgeUnit
		if age >= threshold {
			w.transition(ctx, b, v, rule.NoncurrentVersionTransition.StorageClass, rule.ID)
		}
	}
}

func (w *Worker) expireNoncurrent(ctx context.Context, b *meta.Bucket, v *meta.Object, ruleID string) {
	removed, err := w.Meta.DeleteObject(ctx, b.ID, v.Key, v.VersionID, true)
	if err != nil {
		metrics.LifecycleTickTotal.WithLabelValues("expire_noncurrent", "error").Inc()
		w.Logger.WarnContext(ctx, "lifecycle expire noncurrent", "bucket", b.Name, "key", v.Key, "version", v.VersionID, "error", err.Error())
		return
	}
	region := w.Region
	if region == "" {
		region = "default"
	}
	if removed != nil && removed.Manifest != nil {
		if err := w.Meta.EnqueueChunkDeletion(ctx, region, removed.Manifest.Chunks); err == nil {
			for range removed.Manifest.Chunks {
				metrics.GCEnqueued.Inc()
			}
		}
	}
	metrics.LifecycleExpirations.Inc()
	metrics.LifecycleTickTotal.WithLabelValues("expire_noncurrent", "success").Inc()
	w.Logger.InfoContext(ctx, "lifecycle noncurrent expired", "bucket", b.Name, "key", v.Key, "rule", ruleID, "version", v.VersionID)
}

func (w *Worker) abortStaleUploads(ctx context.Context, b *meta.Bucket, rule *Rule) {
	uploads, err := w.Meta.ListMultipartUploads(ctx, b.ID, rule.PrefixMatch(), 1000)
	if err != nil {
		w.Logger.WarnContext(ctx, "lifecycle list uploads", "bucket", b.Name, "error", err.Error())
		return
	}
	cutoff := time.Now().Add(-time.Duration(rule.AbortIncompleteMultipartUpload.DaysAfterInitiation) * w.AgeUnit)
	region := w.Region
	if region == "" {
		region = "default"
	}
	for _, u := range uploads {
		if u.InitiatedAt.After(cutoff) {
			continue
		}
		manifests, err := w.Meta.AbortMultipartUpload(ctx, b.ID, u.UploadID)
		if err != nil {
			metrics.LifecycleTickTotal.WithLabelValues("abort_multipart", "error").Inc()
			w.Logger.WarnContext(ctx, "lifecycle abort stale upload", "bucket", b.Name, "key", u.Key, "error", err.Error())
			continue
		}
		metrics.LifecycleTickTotal.WithLabelValues("abort_multipart", "success").Inc()
		for _, m := range manifests {
			if m != nil {
				if err := w.Meta.EnqueueChunkDeletion(ctx, region, m.Chunks); err == nil {
					for range m.Chunks {
						metrics.GCEnqueued.Inc()
					}
				}
			}
		}
		w.Logger.InfoContext(ctx, "lifecycle aborted stale multipart", "bucket", b.Name, "key", u.Key, "rule", rule.ID, "upload_id", u.UploadID, "age", time.Since(u.InitiatedAt).String())
	}
}

func (w *Worker) evaluate(ctx context.Context, b *meta.Bucket, rule *Rule, o *meta.Object) {
	age := time.Since(o.Mtime)

	if rule.Expiration != nil && rule.Expiration.Days > 0 {
		if age >= time.Duration(rule.Expiration.Days)*w.AgeUnit {
			w.expire(ctx, b, o, rule.ID)
			return
		}
	}
	if rule.Transition != nil && rule.Transition.Days > 0 && rule.Transition.StorageClass != "" {
		if o.StorageClass == rule.Transition.StorageClass {
			return
		}
		if age >= time.Duration(rule.Transition.Days)*w.AgeUnit {
			w.transition(ctx, b, o, rule.Transition.StorageClass, rule.ID)
		}
	}
}

func (w *Worker) transition(ctx context.Context, b *meta.Bucket, o *meta.Object, newClass, ruleID string) {
	if o.Manifest == nil {
		return
	}
	reader, err := w.Data.GetChunks(ctx, o.Manifest, 0, o.Size)
	if err != nil {
		metrics.LifecycleTickTotal.WithLabelValues("transition", "error").Inc()
		w.Logger.WarnContext(ctx, "lifecycle transition read", "bucket", b.Name, "key", o.Key, "error", err.Error())
		return
	}
	newManifest, err := w.Data.PutChunks(ctx, reader, newClass)
	reader.Close()
	if err != nil {
		metrics.LifecycleTickTotal.WithLabelValues("transition", "error").Inc()
		w.Logger.WarnContext(ctx, "lifecycle transition write", "bucket", b.Name, "key", o.Key, "class", newClass, "error", err.Error())
		return
	}
	applied, err := w.Meta.SetObjectStorage(ctx, b.ID, o.Key, o.VersionID, o.StorageClass, newClass, newManifest)
	region := w.Region
	if region == "" {
		region = "default"
	}
	if err != nil {
		metrics.LifecycleTickTotal.WithLabelValues("transition", "error").Inc()
		w.Logger.WarnContext(ctx, "lifecycle flip manifest", "bucket", b.Name, "key", o.Key, "error", err.Error())
		_ = w.Meta.EnqueueChunkDeletion(ctx, region, newManifest.Chunks)
		return
	}
	if !applied {
		metrics.LifecycleTickTotal.WithLabelValues("transition", "skipped").Inc()
		w.Logger.InfoContext(ctx, "lifecycle race: object modified during transition, discarding", "bucket", b.Name, "key", o.Key, "rule", ruleID)
		_ = w.Meta.EnqueueChunkDeletion(ctx, region, newManifest.Chunks)
		return
	}
	if err := w.Meta.EnqueueChunkDeletion(ctx, region, o.Manifest.Chunks); err != nil {
		w.Logger.WarnContext(ctx, "lifecycle enqueue old chunks", "bucket", b.Name, "key", o.Key, "error", err.Error())
	} else {
		for range o.Manifest.Chunks {
			metrics.GCEnqueued.Inc()
		}
	}
	metrics.LifecycleTransitions.WithLabelValues(newClass).Inc()
	metrics.LifecycleTickTotal.WithLabelValues("transition", "success").Inc()
	w.Logger.InfoContext(ctx, "lifecycle transitioned", "bucket", b.Name, "key", o.Key, "rule", ruleID, "from", o.StorageClass, "to", newClass)
}

func (w *Worker) expire(ctx context.Context, b *meta.Bucket, o *meta.Object, ruleID string) {
	versioned := meta.IsVersioningActive(b.Versioning)
	removed, err := w.Meta.DeleteObject(ctx, b.ID, o.Key, "", versioned)
	if err != nil {
		metrics.LifecycleTickTotal.WithLabelValues("expire", "error").Inc()
		w.Logger.WarnContext(ctx, "lifecycle expire", "bucket", b.Name, "key", o.Key, "error", err.Error())
		return
	}
	if !versioned && removed != nil && removed.Manifest != nil {
		region := w.Region
		if region == "" {
			region = "default"
		}
		if err := w.Meta.EnqueueChunkDeletion(ctx, region, removed.Manifest.Chunks); err != nil {
			w.Logger.WarnContext(ctx, "lifecycle enqueue chunks", "bucket", b.Name, "key", o.Key, "error", err.Error())
		} else {
			for range removed.Manifest.Chunks {
				metrics.GCEnqueued.Inc()
			}
		}
	}
	metrics.LifecycleExpirations.Inc()
	metrics.LifecycleTickTotal.WithLabelValues("expire", "success").Inc()
	w.Logger.InfoContext(ctx, "lifecycle expired", "bucket", b.Name, "key", o.Key, "rule", ruleID, "versioned", versioned)
}
