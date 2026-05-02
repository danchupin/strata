package notify

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danchupin/strata/internal/meta"
	metamem "github.com/danchupin/strata/internal/meta/memory"
)

type fakeSink struct {
	name      string
	mu        sync.Mutex
	failsLeft int
	failErr   error
	calls     int
	bodies    [][]byte
}

func (f *fakeSink) Type() string { return "fake" }
func (f *fakeSink) Name() string { return f.name }
func (f *fakeSink) Send(ctx context.Context, evt meta.NotificationEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.bodies = append(f.bodies, append([]byte(nil), evt.Payload...))
	if f.failsLeft > 0 {
		f.failsLeft--
		return f.failErr
	}
	return nil
}

func newWorkerHarness(t *testing.T, sink *fakeSink, opts ...func(*Config)) (*Worker, *metamem.Store, *meta.Bucket) {
	t.Helper()
	store := metamem.New()
	b, err := store.CreateBucket(context.Background(), "nfy", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	cfg := Config{
		Meta:        store,
		Router:      StaticRouter{"fake:arn:test": sink},
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Interval:    50 * time.Millisecond,
		MaxRetries:  3,
		BackoffBase: 0, // each attempt eligible immediately, controlled via Now/Backoff override below
		Backoff:     func(attempt int) time.Duration { return 0 },
	}
	for _, o := range opts {
		o(&cfg)
	}
	w, err := New(cfg)
	if err != nil {
		t.Fatalf("new worker: %v", err)
	}
	return w, store, b
}

func enqueueEvent(t *testing.T, store *metamem.Store, b *meta.Bucket, eventID string) meta.NotificationEvent {
	t.Helper()
	evt := &meta.NotificationEvent{
		BucketID:   b.ID,
		Bucket:     b.Name,
		Key:        "img/cat.jpg",
		EventID:    eventID,
		EventName:  "s3:ObjectCreated:Put",
		EventTime:  time.Now().UTC(),
		ConfigID:   "OnPut",
		TargetType: "fake",
		TargetARN:  "arn:test",
		Payload:    []byte(`{"Records":[{"eventName":"s3:ObjectCreated:Put"}]}`),
	}
	if err := store.EnqueueNotification(context.Background(), evt); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	return *evt
}

func TestWorkerDeliversAndAcksOnSuccess(t *testing.T) {
	sink := &fakeSink{name: "ok"}
	w, store, b := newWorkerHarness(t, sink)
	enqueueEvent(t, store, b, "evt-success")

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("run once: %v", err)
	}
	if sink.calls != 1 {
		t.Fatalf("calls=%d want 1", sink.calls)
	}
	pending, _ := store.ListPendingNotifications(context.Background(), b.ID, 100)
	if len(pending) != 0 {
		t.Fatalf("pending after success: %d", len(pending))
	}
	dlq, _ := store.ListNotificationDLQ(context.Background(), b.ID, 100)
	if len(dlq) != 0 {
		t.Fatalf("dlq after success: %d", len(dlq))
	}
}

func TestWorkerRetriesThenDLQ(t *testing.T) {
	sink := &fakeSink{name: "flaky", failsLeft: 999, failErr: errors.New("upstream 502")}
	w, store, b := newWorkerHarness(t, sink, func(c *Config) {
		c.MaxRetries = 6
	})
	enqueueEvent(t, store, b, "evt-retry")

	for i := 0; i < 6; i++ {
		if err := w.RunOnce(context.Background()); err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
	}
	if sink.calls != 6 {
		t.Fatalf("send calls=%d want 6", sink.calls)
	}
	pending, _ := store.ListPendingNotifications(context.Background(), b.ID, 100)
	if len(pending) != 0 {
		t.Fatalf("pending after dlq: %d", len(pending))
	}
	dlq, _ := store.ListNotificationDLQ(context.Background(), b.ID, 100)
	if len(dlq) != 1 {
		t.Fatalf("dlq len=%d want 1", len(dlq))
	}
	if dlq[0].Attempts != 6 || dlq[0].Reason == "" {
		t.Fatalf("dlq entry: %+v", dlq[0])
	}
	if dlq[0].EventID != "evt-retry" {
		t.Fatalf("dlq event id %q", dlq[0].EventID)
	}
}

func TestWorkerSucceedsAfterTransientFailures(t *testing.T) {
	sink := &fakeSink{name: "flaky", failsLeft: 2, failErr: errors.New("upstream 503")}
	w, store, b := newWorkerHarness(t, sink, func(c *Config) {
		c.MaxRetries = 6
	})
	enqueueEvent(t, store, b, "evt-flaky")

	for i := 0; i < 5; i++ {
		_ = w.RunOnce(context.Background())
	}
	if sink.calls != 3 {
		t.Fatalf("send calls=%d want 3 (2 fail + 1 success)", sink.calls)
	}
	pending, _ := store.ListPendingNotifications(context.Background(), b.ID, 100)
	if len(pending) != 0 {
		t.Fatalf("pending after recovery: %d", len(pending))
	}
	dlq, _ := store.ListNotificationDLQ(context.Background(), b.ID, 100)
	if len(dlq) != 0 {
		t.Fatalf("dlq after recovery: %d", len(dlq))
	}
}

func TestWorkerRoutesUnknownTargetToDLQ(t *testing.T) {
	sink := &fakeSink{name: "ok"}
	w, store, b := newWorkerHarness(t, sink)
	evt := &meta.NotificationEvent{
		BucketID:   b.ID,
		Bucket:     b.Name,
		Key:        "x",
		EventID:    "evt-orphan",
		EventName:  "s3:ObjectCreated:Put",
		EventTime:  time.Now().UTC(),
		ConfigID:   "OnPut",
		TargetType: "fake",
		TargetARN:  "arn:does-not-exist",
		Payload:    []byte(`{}`),
	}
	if err := store.EnqueueNotification(context.Background(), evt); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("run once: %v", err)
	}
	if sink.calls != 0 {
		t.Fatalf("orphan event should not hit sink (calls=%d)", sink.calls)
	}
	dlq, _ := store.ListNotificationDLQ(context.Background(), b.ID, 100)
	if len(dlq) != 1 {
		t.Fatalf("orphan should land in DLQ, got %d", len(dlq))
	}
	if dlq[0].Reason != "no sink registered for target" {
		t.Fatalf("dlq reason: %q", dlq[0].Reason)
	}
}

func TestWorkerRunStopsOnContextCancel(t *testing.T) {
	sink := &fakeSink{name: "ok"}
	w, _, _ := newWorkerHarness(t, sink, func(c *Config) {
		c.Interval = 10 * time.Millisecond
	})
	ctx, cancel := context.WithCancel(context.Background())
	var stopped atomic.Bool
	go func() {
		_ = w.Run(ctx)
		stopped.Store(true)
	}()
	time.Sleep(30 * time.Millisecond)
	cancel()
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if stopped.Load() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("Run did not return after ctx cancel")
}

func TestWorkerHonoursBackoffWindow(t *testing.T) {
	sink := &fakeSink{name: "flaky", failsLeft: 999, failErr: errors.New("nope")}
	now := time.Unix(1_700_000_000, 0)
	clock := &now
	w, store, b := newWorkerHarness(t, sink, func(c *Config) {
		c.MaxRetries = 5
		c.Now = func() time.Time { return *clock }
		c.Backoff = func(attempt int) time.Duration { return 30 * time.Second }
	})
	enqueueEvent(t, store, b, "evt-backoff")

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("first tick: %v", err)
	}
	if sink.calls != 1 {
		t.Fatalf("first tick calls=%d want 1", sink.calls)
	}
	// Second tick before backoff elapses should NOT re-attempt.
	*clock = clock.Add(5 * time.Second)
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if sink.calls != 1 {
		t.Fatalf("second tick should be skipped, calls=%d", sink.calls)
	}
	// Advance past backoff window — sink should be called again.
	*clock = clock.Add(60 * time.Second)
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("third tick: %v", err)
	}
	if sink.calls != 2 {
		t.Fatalf("third tick should re-attempt, calls=%d want 2", sink.calls)
	}
}
