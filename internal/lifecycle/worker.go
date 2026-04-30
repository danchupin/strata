package lifecycle

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/danchupin/strata/internal/data"
	datas3 "github.com/danchupin/strata/internal/data/s3"
	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/metrics"
)

type Worker struct {
	Meta     meta.Store
	Data     data.Backend
	Region   string
	Interval time.Duration
	AgeUnit  time.Duration
	Logger   *log.Logger
}

func (w *Worker) Run(ctx context.Context) error {
	if w.Interval == 0 {
		w.Interval = 60 * time.Second
	}
	if w.AgeUnit == 0 {
		w.AgeUnit = 24 * time.Hour
	}
	if w.Logger == nil {
		w.Logger = log.Default()
	}
	w.Logger.Printf("lifecycle: starting (interval=%s age-unit=%s)", w.Interval, w.AgeUnit)

	ticker := time.NewTicker(w.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := w.runOnce(ctx); err != nil {
				w.Logger.Printf("lifecycle: tick failed: %v", err)
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
			w.Logger.Printf("lifecycle: bucket %s: get rules: %v", b.Name, err)
			continue
		}
		cfg, err := Parse(blob)
		if err != nil {
			w.Logger.Printf("lifecycle: bucket %s: parse: %v", b.Name, err)
			continue
		}
		for i := range cfg.Rules {
			rule := &cfg.Rules[i]
			if !rule.IsEnabled() {
				continue
			}
			if err := w.applyRule(ctx, b, rule); err != nil {
				w.Logger.Printf("lifecycle: bucket %s rule %q: %v", b.Name, rule.ID, err)
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
		// US-014: noncurrent-transition translation isn't wired through
		// LifecycleBackend yet — the worker still owns it. When that lands,
		// add the same w.backendOwnsTransition() short-circuit as evaluate.
		threshold := time.Duration(rule.NoncurrentVersionTransition.NoncurrentDays) * w.AgeUnit
		if age >= threshold {
			w.transition(ctx, b, v, rule.NoncurrentVersionTransition.StorageClass, rule.ID)
		}
	}
}

func (w *Worker) expireNoncurrent(ctx context.Context, b *meta.Bucket, v *meta.Object, ruleID string) {
	removed, err := w.Meta.DeleteObject(ctx, b.ID, v.Key, v.VersionID, true)
	if err != nil {
		w.Logger.Printf("lifecycle: %s/%s version %s expire: %v", b.Name, v.Key, v.VersionID, err)
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
	w.Logger.Printf("lifecycle: %s/%s [%s] noncurrent version %s expired", b.Name, v.Key, ruleID, v.VersionID)
}

func (w *Worker) abortStaleUploads(ctx context.Context, b *meta.Bucket, rule *Rule) {
	uploads, err := w.Meta.ListMultipartUploads(ctx, b.ID, rule.PrefixMatch(), 1000)
	if err != nil {
		w.Logger.Printf("lifecycle: %s list uploads: %v", b.Name, err)
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
			w.Logger.Printf("lifecycle: %s/%s abort stale upload: %v", b.Name, u.Key, err)
			continue
		}
		for _, m := range manifests {
			if m != nil {
				if err := w.Meta.EnqueueChunkDeletion(ctx, region, m.Chunks); err == nil {
					for range m.Chunks {
						metrics.GCEnqueued.Inc()
					}
				}
			}
		}
		w.Logger.Printf("lifecycle: %s/%s [%s] aborted stale multipart %s (age %s)", b.Name, u.Key, rule.ID, u.UploadID, time.Since(u.InitiatedAt))
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
		// US-014: when the data backend translates this transition into a
		// native backend lifecycle rule (s3 backend + native storage class),
		// the worker no longer owns the transition — skip it to avoid
		// double-work and a redundant rewrite of the manifest's storage
		// class.
		if w.backendOwnsTransition(rule.Transition.StorageClass) {
			return
		}
		if o.StorageClass == rule.Transition.StorageClass {
			return
		}
		if age >= time.Duration(rule.Transition.Days)*w.AgeUnit {
			w.transition(ctx, b, o, rule.Transition.StorageClass, rule.ID)
		}
	}
}

// backendOwnsTransition reports whether the configured data backend
// translates a transition to the supplied storage class into a native
// backend lifecycle rule (US-014). Today only the s3 backend implements
// LifecycleBackend AND only for the AWS-native classes documented in
// docs/backends/s3.md.
func (w *Worker) backendOwnsTransition(class string) bool {
	if _, ok := w.Data.(data.LifecycleBackend); !ok {
		return false
	}
	return datas3.IsNativeTransitionClass(class)
}

func (w *Worker) transition(ctx context.Context, b *meta.Bucket, o *meta.Object, newClass, ruleID string) {
	if o.Manifest == nil {
		return
	}
	reader, err := w.Data.GetChunks(ctx, o.Manifest, 0, o.Size)
	if err != nil {
		w.Logger.Printf("lifecycle: %s/%s transition read: %v", b.Name, o.Key, err)
		return
	}
	newManifest, err := w.Data.PutChunks(ctx, reader, newClass)
	reader.Close()
	if err != nil {
		w.Logger.Printf("lifecycle: %s/%s transition write %s: %v", b.Name, o.Key, newClass, err)
		return
	}
	applied, err := w.Meta.SetObjectStorage(ctx, b.ID, o.Key, o.VersionID, o.StorageClass, newClass, newManifest)
	region := w.Region
	if region == "" {
		region = "default"
	}
	if err != nil {
		w.Logger.Printf("lifecycle: %s/%s flip manifest: %v", b.Name, o.Key, err)
		_ = w.Meta.EnqueueChunkDeletion(ctx, region, newManifest.Chunks)
		return
	}
	if !applied {
		w.Logger.Printf("lifecycle: %s/%s [%s] race: object modified during transition, discarding", b.Name, o.Key, ruleID)
		_ = w.Meta.EnqueueChunkDeletion(ctx, region, newManifest.Chunks)
		return
	}
	if err := w.Meta.EnqueueChunkDeletion(ctx, region, o.Manifest.Chunks); err != nil {
		w.Logger.Printf("lifecycle: %s/%s enqueue old chunks: %v", b.Name, o.Key, err)
	} else {
		for range o.Manifest.Chunks {
			metrics.GCEnqueued.Inc()
		}
	}
	metrics.LifecycleTransitions.WithLabelValues(newClass).Inc()
	w.Logger.Printf("lifecycle: %s/%s [%s] %s -> %s", b.Name, o.Key, ruleID, o.StorageClass, newClass)
}

func (w *Worker) expire(ctx context.Context, b *meta.Bucket, o *meta.Object, ruleID string) {
	versioned := meta.IsVersioningActive(b.Versioning)
	removed, err := w.Meta.DeleteObject(ctx, b.ID, o.Key, "", versioned)
	if err != nil {
		w.Logger.Printf("lifecycle: %s/%s expire: %v", b.Name, o.Key, err)
		return
	}
	if !versioned && removed != nil && removed.Manifest != nil {
		region := w.Region
		if region == "" {
			region = "default"
		}
		if err := w.Meta.EnqueueChunkDeletion(ctx, region, removed.Manifest.Chunks); err != nil {
			w.Logger.Printf("lifecycle: %s/%s enqueue chunks: %v", b.Name, o.Key, err)
		} else {
			for range removed.Manifest.Chunks {
				metrics.GCEnqueued.Inc()
			}
		}
	}
	metrics.LifecycleExpirations.Inc()
	w.Logger.Printf("lifecycle: %s/%s [%s] expired (versioned=%v)", b.Name, o.Key, ruleID, versioned)
}
