package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/meta"
)

// Config wires a Worker. Defaults applied in New: Interval=5s, MaxRetries=6,
// BackoffBase=1s, Now=time.Now, Logger=slog.Default. PollLimit caps how many
// rows are pulled per bucket per tick.
type Config struct {
	Meta        meta.Store
	Router      Router
	Logger      *slog.Logger
	Interval    time.Duration
	MaxRetries  int
	BackoffBase time.Duration
	Backoff     func(attempt int) time.Duration
	PollLimit   int
	Now         func() time.Time
}

// Worker drains notify_queue rows, dispatches each to its Router-resolved
// sink, retries with exponential backoff on failure, and moves rows to
// notify_dlq after MaxRetries failed attempts.
type Worker struct {
	cfg Config

	mu          sync.Mutex
	attempts    map[string]int
	lastError   map[string]string
	nextAttempt map[string]time.Time
}

func New(cfg Config) (*Worker, error) {
	if cfg.Meta == nil {
		return nil, errors.New("notify: meta store required")
	}
	if cfg.Router == nil {
		return nil, errors.New("notify: router required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
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
	w.cfg.Logger.Info("notify: starting", "interval", w.cfg.Interval, "max_retries", w.cfg.MaxRetries)
	ticker := time.NewTicker(w.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := w.RunOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
				w.cfg.Logger.Warn("notify: tick failed", "error", err.Error())
			}
		}
	}
}

// RunOnce performs a single poll-and-dispatch pass over every bucket.
// Exposed for tests and for cmd/strata-notify --once.
func (w *Worker) RunOnce(ctx context.Context) error {
	buckets, err := w.cfg.Meta.ListBuckets(ctx, "")
	if err != nil {
		return fmt.Errorf("list buckets: %w", err)
	}
	for _, b := range buckets {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := w.drainBucket(ctx, b.ID); err != nil {
			w.cfg.Logger.Warn("notify: drain bucket failed", "bucket", b.Name, "error", err.Error())
		}
	}
	return nil
}

func (w *Worker) drainBucket(ctx context.Context, bucketID uuid.UUID) error {
	events, err := w.cfg.Meta.ListPendingNotifications(ctx, bucketID, w.cfg.PollLimit)
	if err != nil {
		return err
	}
	now := w.cfg.Now()
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

func (w *Worker) dispatch(ctx context.Context, evt meta.NotificationEvent) {
	sink, ok := w.cfg.Router.Resolve(evt)
	if !ok {
		w.cfg.Logger.Warn("notify: no sink for event; moving to DLQ",
			"event_id", evt.EventID, "target_type", evt.TargetType, "target_arn", evt.TargetARN)
		w.toDLQ(ctx, evt, 0, "no sink registered for target")
		return
	}
	err := sink.Send(ctx, evt)
	if err == nil {
		w.success(ctx, evt, sink)
		return
	}
	w.failure(ctx, evt, sink, err)
}

func (w *Worker) success(ctx context.Context, evt meta.NotificationEvent, sink Sink) {
	w.mu.Lock()
	delete(w.attempts, evt.EventID)
	delete(w.lastError, evt.EventID)
	delete(w.nextAttempt, evt.EventID)
	w.mu.Unlock()
	if err := w.cfg.Meta.AckNotification(ctx, evt); err != nil {
		w.cfg.Logger.Warn("notify: ack failed", "event_id", evt.EventID, "error", err.Error())
		return
	}
	w.cfg.Logger.Info("notify: delivered",
		"event_id", evt.EventID, "sink", sink.Name(), "type", sink.Type(), "event", evt.EventName)
}

func (w *Worker) failure(ctx context.Context, evt meta.NotificationEvent, sink Sink, sendErr error) {
	w.mu.Lock()
	w.attempts[evt.EventID]++
	attempts := w.attempts[evt.EventID]
	w.lastError[evt.EventID] = sendErr.Error()
	if attempts >= w.cfg.MaxRetries {
		delete(w.attempts, evt.EventID)
		delete(w.lastError, evt.EventID)
		delete(w.nextAttempt, evt.EventID)
		w.mu.Unlock()
		w.cfg.Logger.Warn("notify: retry budget exhausted; moving to DLQ",
			"event_id", evt.EventID, "sink", sink.Name(), "attempts", attempts, "error", sendErr.Error())
		w.toDLQ(ctx, evt, attempts, sendErr.Error())
		return
	}
	w.nextAttempt[evt.EventID] = w.cfg.Now().Add(w.cfg.Backoff(attempts))
	w.mu.Unlock()
	w.cfg.Logger.Warn("notify: delivery failed; will retry",
		"event_id", evt.EventID, "sink", sink.Name(), "attempts", attempts, "error", sendErr.Error())
}

func (w *Worker) toDLQ(ctx context.Context, evt meta.NotificationEvent, attempts int, reason string) {
	entry := &meta.NotificationDLQEntry{
		NotificationEvent: evt,
		Attempts:          attempts,
		Reason:            reason,
		EnqueuedAt:        w.cfg.Now(),
	}
	if err := w.cfg.Meta.EnqueueNotificationDLQ(ctx, entry); err != nil {
		w.cfg.Logger.Warn("notify: dlq enqueue failed", "event_id", evt.EventID, "error", err.Error())
		return
	}
	if err := w.cfg.Meta.AckNotification(ctx, evt); err != nil {
		w.cfg.Logger.Warn("notify: dlq ack failed", "event_id", evt.EventID, "error", err.Error())
	}
}
