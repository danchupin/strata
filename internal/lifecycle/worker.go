package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/danchupin/strata/internal/data"
	datas3 "github.com/danchupin/strata/internal/data/s3"
	"github.com/danchupin/strata/internal/leader"
	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/metrics"
)

// DefaultBucketLeaseTTL is the per-bucket lease TTL when LeaderTTL is unset.
// Short enough that a crashed replica's hold rolls over within a cycle,
// long enough that the renewing lease covers a worst-case bucket scan.
const DefaultBucketLeaseTTL = 60 * time.Second

// LeaseName returns the per-bucket lifecycle lease key. Public so operator
// dashboards / runbooks don't have to hard-code the format string.
func LeaseName(bucketID string) string { return fmt.Sprintf("lifecycle-leader-%s", bucketID) }

// bucketReplicaIndex maps a bucketID to the replica index that owns it under
// the Phase 2 distribution gate. Stable: changing the hash function would
// re-partition active leases mid-flight and double-process buckets.
func bucketReplicaIndex(bucketID string, replicaCount int) int {
	if replicaCount <= 0 {
		return 0
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(bucketID))
	return int(h.Sum32() % uint32(replicaCount))
}

type Worker struct {
	Meta        meta.Store
	Data        data.Backend
	Region      string
	Interval    time.Duration
	AgeUnit     time.Duration
	Concurrency int
	Logger      *slog.Logger

	// Locker + ReplicaInfo switch the worker into Phase 2 per-bucket lease
	// mode (US-005). When both are set, every cycle walks buckets, applies
	// the `fnv32a(bucketID) % count == id` distribution gate, then attempts
	// a non-blocking `lifecycle-leader-<bucketID>` lease before processing
	// the bucket. Buckets where the lease is already held elsewhere (or
	// that fail the gate) are skipped — a sibling replica owns that bucket
	// this cycle.
	//
	// Leaving Locker or ReplicaInfo nil falls back to the legacy single-
	// replica path: every bucket processed sequentially, no per-bucket
	// lease, no distribution gate. That path is what the admin/bench
	// callers use when invoking RunOnce directly.
	Locker leader.Locker
	// ReplicaInfo returns (count, id) where count is the number of replica
	// slots (typically STRATA_GC_SHARDS) and id is this replica's index
	// within that ring. Returning count<=0 disables the distribution gate
	// for this cycle (every bucket eligible). Returning id<0 with count>0
	// skips lifecycle work entirely — the replica has no defensible stake
	// in any bucket subset right now (e.g. no gc shard held).
	ReplicaInfo func() (count int, id int)
	// LeaderTTL is the per-bucket lease TTL. Zero falls back to
	// DefaultBucketLeaseTTL. The bucket lease is held only for the duration
	// of one bucket's processing then released, so no renew loop is needed
	// — the TTL is a crash-recovery upper bound, not a steady-state hold.
	LeaderTTL time.Duration
}

// effectiveConcurrency clamps Concurrency to [1, 256]; zero/negative -> 1.
func (w *Worker) effectiveConcurrency() int {
	c := w.Concurrency
	if c < 1 {
		return 1
	}
	if c > 256 {
		return 256
	}
	return c
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
	w.Logger.InfoContext(ctx, "lifecycle: starting", "interval", w.Interval.String(), "age_unit", w.AgeUnit.String(), "concurrency", w.effectiveConcurrency())

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

// distributedMode reports whether the Phase-2 per-bucket lease layer is
// wired. Locker AND ReplicaInfo must both be set; otherwise the worker runs
// in the legacy single-replica path used by admin/bench callers.
func (w *Worker) distributedMode() bool {
	return w.Locker != nil && w.ReplicaInfo != nil
}

func (w *Worker) runOnce(ctx context.Context) error {
	buckets, err := w.Meta.ListBuckets(ctx, "")
	if err != nil {
		return err
	}

	distributed := w.distributedMode()
	var (
		replicaCount int
		myReplicaID  int
	)
	if distributed {
		replicaCount, myReplicaID = w.ReplicaInfo()
		if replicaCount > 0 && myReplicaID < 0 {
			w.Logger.DebugContext(ctx, "lifecycle skip cycle: no replica index (gc shards not held)")
			return nil
		}
	}

	for _, b := range buckets {
		if distributed && replicaCount > 0 && bucketReplicaIndex(b.ID.String(), replicaCount) != myReplicaID {
			continue
		}

		release, owned, err := w.acquireBucketLease(ctx, b)
		if err != nil {
			w.Logger.WarnContext(ctx, "lifecycle bucket lease", "bucket", b.Name, "error", err.Error())
			continue
		}
		if !owned {
			continue
		}

		w.processBucket(ctx, b)
		release()
	}
	return nil
}

// acquireBucketLease grabs the per-bucket lease in distributed mode. Returns
// owned=true with a release closure on success; owned=false (with a no-op
// release) when another replica holds the lease this cycle or when the
// worker is in the legacy single-replica path.
func (w *Worker) acquireBucketLease(ctx context.Context, b *meta.Bucket) (release func(), owned bool, err error) {
	if !w.distributedMode() {
		return func() {}, true, nil
	}
	ttl := w.LeaderTTL
	if ttl == 0 {
		ttl = DefaultBucketLeaseTTL
	}
	holder := leader.DefaultHolder()
	name := LeaseName(b.ID.String())
	got, err := w.Locker.Acquire(ctx, name, holder, ttl)
	if err != nil {
		return nil, false, err
	}
	if !got {
		return func() {}, false, nil
	}
	release = func() {
		if relErr := w.Locker.Release(context.Background(), name, holder); relErr != nil {
			w.Logger.WarnContext(ctx, "lifecycle bucket release", "bucket", b.Name, "error", relErr.Error())
		}
	}
	return release, true, nil
}

func (w *Worker) processBucket(ctx context.Context, b *meta.Bucket) {
	blob, err := w.Meta.GetBucketLifecycle(ctx, b.ID)
	if err != nil {
		if errors.Is(err, meta.ErrNoSuchLifecycle) {
			return
		}
		w.Logger.WarnContext(ctx, "lifecycle get rules", "bucket", b.Name, "error", err.Error())
		return
	}
	cfg, err := Parse(blob)
	if err != nil {
		w.Logger.WarnContext(ctx, "lifecycle parse", "bucket", b.Name, "error", err.Error())
		return
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
	limit := w.effectiveConcurrency()
	opts := meta.ListOptions{Prefix: rule.PrefixMatch(), Limit: 1000}
	for {
		res, err := w.Meta.ListObjects(ctx, b.ID, opts)
		if err != nil {
			return err
		}
		eg := new(errgroup.Group)
		eg.SetLimit(limit)
		for _, o := range res.Objects {
			eg.Go(func() error {
				defer func() {
					if r := recover(); r != nil {
						w.Logger.WarnContext(ctx, "lifecycle evaluate panic",
							"bucket", b.Name, "key", o.Key, "panic", r)
					}
				}()
				w.evaluate(ctx, b, rule, o)
				return nil
			})
		}
		_ = eg.Wait()
		if !res.Truncated {
			return nil
		}
		opts.Marker = res.NextMarker
	}
}

func (w *Worker) applyNoncurrentActions(ctx context.Context, b *meta.Bucket, rule *Rule) error {
	limit := w.effectiveConcurrency()
	opts := meta.ListOptions{Prefix: rule.PrefixMatch(), Limit: 1000}
	for {
		res, err := w.Meta.ListObjectVersions(ctx, b.ID, opts)
		if err != nil {
			return err
		}
		// Walk versions sequentially to track per-key prevMtime; dispatch
		// each (v, age) pair to the bounded errgroup once age is known.
		type ncJob struct {
			v   *meta.Object
			age time.Duration
		}
		var jobs []ncJob
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
			jobs = append(jobs, ncJob{v: v, age: time.Since(noncurrentSince)})
		}
		eg := new(errgroup.Group)
		eg.SetLimit(limit)
		for _, j := range jobs {
			eg.Go(func() error {
				defer func() {
					if r := recover(); r != nil {
						w.Logger.WarnContext(ctx, "lifecycle noncurrent panic",
							"bucket", b.Name, "key", j.v.Key, "version", j.v.VersionID, "panic", r)
					}
				}()
				w.evaluateNoncurrent(ctx, b, rule, j.v, j.age)
				return nil
			})
		}
		_ = eg.Wait()
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
	limit := w.effectiveConcurrency()
	eg := new(errgroup.Group)
	eg.SetLimit(limit)
	for _, u := range uploads {
		if u.InitiatedAt.After(cutoff) {
			continue
		}
		eg.Go(func() error {
			defer func() {
				if r := recover(); r != nil {
					w.Logger.WarnContext(ctx, "lifecycle abort panic",
						"bucket", b.Name, "key", u.Key, "upload_id", u.UploadID, "panic", r)
				}
			}()
			manifests, err := w.Meta.AbortMultipartUpload(ctx, b.ID, u.UploadID)
			if err != nil {
				metrics.LifecycleTickTotal.WithLabelValues("abort_multipart", "error").Inc()
				w.Logger.WarnContext(ctx, "lifecycle abort stale upload", "bucket", b.Name, "key", u.Key, "error", err.Error())
				return nil
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
			return nil
		})
	}
	_ = eg.Wait()
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
