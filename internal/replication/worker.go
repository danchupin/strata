package replication

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/meta"
)

// ReplicationStatusCompleted / ReplicationStatusFailed are the literal AWS
// values written back to the source object's replication-status column after
// a delivery succeeds or exhausts its retry budget.
const (
	ReplicationStatusCompleted = "COMPLETED"
	ReplicationStatusFailed    = "FAILED"
)

// MetricsObserver lets the cmd binary plug a Prometheus histogram for
// replication_lag_seconds without forcing internal/replication to import the
// prometheus client. The worker observes lag = now - evt.EventTime on every
// terminal outcome (success or DLQ-equivalent FAILED) so dashboards see both
// happy + sad paths.
type MetricsObserver interface {
	ObserveLag(ruleID string, lagSeconds float64)
	IncCompleted(ruleID string)
	IncFailed(ruleID string)
	SetQueueDepth(ruleID string, depth int)
	// SetQueueAge publishes the oldest pending replication_queue row age
	// (seconds) for a source bucket. Sampled at every drain tick — including
	// 0 for buckets with an empty queue so the per-bucket Replication tab
	// (US-014) sees a flatline instead of staleness.
	SetQueueAge(bucket string, ageSeconds float64)
}

// nopMetrics is a no-op observer used when cfg.Metrics is nil.
type nopMetrics struct{}

func (nopMetrics) ObserveLag(string, float64)   {}
func (nopMetrics) IncCompleted(string)          {}
func (nopMetrics) IncFailed(string)             {}
func (nopMetrics) SetQueueDepth(string, int)    {}
func (nopMetrics) SetQueueAge(string, float64)  {}

// Config wires the replicator Worker. Defaults applied in New: Interval=5s,
// MaxRetries=6, BackoffBase=1s, Now=time.Now, Logger=slog.Default,
// PollLimit=100, Metrics=nopMetrics.
type Config struct {
	Meta        meta.Store
	Data        data.Backend
	Dispatcher  Dispatcher
	Logger      *slog.Logger
	Metrics     MetricsObserver
	Interval    time.Duration
	MaxRetries  int
	BackoffBase time.Duration
	Backoff     func(attempt int) time.Duration
	PollLimit   int
	Now         func() time.Time
}

// Worker drains replication_queue rows, reads each source object via Meta+Data,
// dispatches via Dispatcher, and updates the source's replication_status
// column. After MaxRetries failures it writes FAILED + observes lag.
type Worker struct {
	cfg Config

	mu          sync.Mutex
	attempts    map[string]int
	lastError   map[string]string
	nextAttempt map[string]time.Time
}

func New(cfg Config) (*Worker, error) {
	if cfg.Meta == nil {
		return nil, errors.New("replication: meta store required")
	}
	if cfg.Data == nil {
		return nil, errors.New("replication: data backend required")
	}
	if cfg.Dispatcher == nil {
		return nil, errors.New("replication: dispatcher required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Metrics == nil {
		cfg.Metrics = nopMetrics{}
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 5 * time.Second
	}
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = 6
	}
	if cfg.BackoffBase <= 0 {
		cfg.BackoffBase = 1 * time.Second
	}
	if cfg.PollLimit <= 0 {
		cfg.PollLimit = 100
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Backoff == nil {
		base := cfg.BackoffBase
		cfg.Backoff = func(attempt int) time.Duration {
			d := base
			for i := 1; i < attempt; i++ {
				d *= 2
				if d > 5*time.Minute {
					return 5 * time.Minute
				}
			}
			return d
		}
	}
	return &Worker{
		cfg:         cfg,
		attempts:    make(map[string]int),
		lastError:   make(map[string]string),
		nextAttempt: make(map[string]time.Time),
	}, nil
}

// Run loops on cfg.Interval, draining queues until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) error {
	w.cfg.Logger.Info("replication: starting", "interval", w.cfg.Interval, "max_retries", w.cfg.MaxRetries)
	ticker := time.NewTicker(w.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := w.RunOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
				w.cfg.Logger.Warn("replication: tick failed", "error", err.Error())
			}
		}
	}
}

// RunOnce performs a single poll-and-dispatch pass over every bucket. Exposed
// for tests + the cmd binary's --once flag.
func (w *Worker) RunOnce(ctx context.Context) error {
	buckets, err := w.cfg.Meta.ListBuckets(ctx, "")
	if err != nil {
		return fmt.Errorf("list buckets: %w", err)
	}
	for _, b := range buckets {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := w.drainBucket(ctx, b.ID, b.Name); err != nil {
			w.cfg.Logger.Warn("replication: drain bucket failed", "bucket", b.Name, "error", err.Error())
		}
	}
	return nil
}

func (w *Worker) drainBucket(ctx context.Context, bucketID uuid.UUID, bucketName string) error {
	events, err := w.cfg.Meta.ListPendingReplications(ctx, bucketID, w.cfg.PollLimit)
	if err != nil {
		return err
	}
	now := w.cfg.Now()
	// Per-bucket queue age: oldest pending event_time delta. Emitted every
	// tick (including 0 when empty) so the per-bucket Replication tab
	// (US-014) sees a flatline rather than a stale gauge.
	if len(events) == 0 {
		w.cfg.Metrics.SetQueueAge(bucketName, 0)
	} else {
		depths := map[string]int{}
		oldest := events[0].EventTime
		for _, evt := range events {
			depths[evt.RuleID]++
			if evt.EventTime.Before(oldest) {
				oldest = evt.EventTime
			}
		}
		for rule, n := range depths {
			w.cfg.Metrics.SetQueueDepth(rule, n)
		}
		age := now.Sub(oldest).Seconds()
		if age < 0 {
			age = 0
		}
		w.cfg.Metrics.SetQueueAge(bucketName, age)
	}
	for _, evt := range events {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !w.dueNow(evt.EventID, now) {
			continue
		}
		w.dispatch(ctx, evt)
	}
	return nil
}

func (w *Worker) dueNow(eventID string, now time.Time) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	next, ok := w.nextAttempt[eventID]
	if !ok {
		return true
	}
	return !now.Before(next)
}

func (w *Worker) dispatch(ctx context.Context, evt meta.ReplicationEvent) {
	src, err := w.loadSource(ctx, evt)
	if err != nil {
		w.failure(ctx, evt, fmt.Errorf("load source: %w", err))
		return
	}
	if err := w.cfg.Dispatcher.Send(ctx, evt, src); err != nil {
		w.failure(ctx, evt, err)
		return
	}
	w.success(ctx, evt)
}

func (w *Worker) loadSource(ctx context.Context, evt meta.ReplicationEvent) (*Source, error) {
	o, err := w.cfg.Meta.GetObject(ctx, evt.BucketID, evt.Key, evt.VersionID)
	if err != nil {
		return nil, err
	}
	if o.Manifest == nil {
		return nil, errors.New("source object has no manifest")
	}
	body, err := w.cfg.Data.GetChunks(ctx, o.Manifest, 0, o.Size)
	if err != nil {
		return nil, err
	}
	return &Source{
		Body:         body,
		Size:         o.Size,
		ContentType:  o.ContentType,
		StorageClass: o.StorageClass,
		UserMeta:     o.UserMeta,
	}, nil
}

func (w *Worker) success(ctx context.Context, evt meta.ReplicationEvent) {
	w.mu.Lock()
	delete(w.attempts, evt.EventID)
	delete(w.lastError, evt.EventID)
	delete(w.nextAttempt, evt.EventID)
	w.mu.Unlock()
	if err := w.cfg.Meta.SetObjectReplicationStatus(ctx, evt.BucketID, evt.Key, evt.VersionID, ReplicationStatusCompleted); err != nil {
		w.cfg.Logger.Warn("replication: status update failed",
			"event_id", evt.EventID, "error", err.Error())
	}
	if err := w.cfg.Meta.AckReplication(ctx, evt); err != nil {
		w.cfg.Logger.Warn("replication: ack failed",
			"event_id", evt.EventID, "error", err.Error())
		return
	}
	lag := w.cfg.Now().Sub(evt.EventTime).Seconds()
	w.cfg.Metrics.ObserveLag(evt.RuleID, lag)
	w.cfg.Metrics.IncCompleted(evt.RuleID)
	w.cfg.Logger.Info("replication: completed",
		"event_id", evt.EventID, "rule", evt.RuleID, "key", evt.Key,
		"destination", evt.DestinationBucket, "lag_seconds", lag)
}

func (w *Worker) failure(ctx context.Context, evt meta.ReplicationEvent, sendErr error) {
	w.mu.Lock()
	w.attempts[evt.EventID]++
	attempts := w.attempts[evt.EventID]
	w.lastError[evt.EventID] = sendErr.Error()
	if attempts >= w.cfg.MaxRetries {
		delete(w.attempts, evt.EventID)
		delete(w.lastError, evt.EventID)
		delete(w.nextAttempt, evt.EventID)
		w.mu.Unlock()
		w.markFailed(ctx, evt, attempts, sendErr)
		return
	}
	w.nextAttempt[evt.EventID] = w.cfg.Now().Add(w.cfg.Backoff(attempts))
	w.mu.Unlock()
	w.cfg.Logger.Warn("replication: delivery failed; will retry",
		"event_id", evt.EventID, "rule", evt.RuleID, "attempts", attempts, "error", sendErr.Error())
}

func (w *Worker) markFailed(ctx context.Context, evt meta.ReplicationEvent, attempts int, sendErr error) {
	if err := w.cfg.Meta.SetObjectReplicationStatus(ctx, evt.BucketID, evt.Key, evt.VersionID, ReplicationStatusFailed); err != nil {
		w.cfg.Logger.Warn("replication: failed-status update failed",
			"event_id", evt.EventID, "error", err.Error())
	}
	if err := w.cfg.Meta.AckReplication(ctx, evt); err != nil {
		w.cfg.Logger.Warn("replication: ack of failed row failed",
			"event_id", evt.EventID, "error", err.Error())
	}
	lag := w.cfg.Now().Sub(evt.EventTime).Seconds()
	w.cfg.Metrics.ObserveLag(evt.RuleID, lag)
	w.cfg.Metrics.IncFailed(evt.RuleID)
	w.cfg.Logger.Warn("replication: retry budget exhausted; marked FAILED",
		"event_id", evt.EventID, "rule", evt.RuleID, "attempts", attempts, "error", sendErr.Error(), "lag_seconds", lag)
}
