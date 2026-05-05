// Package auditstream is the in-process pub-sub fan-out for live audit-log
// rows (US-001 — Phase 3 debug tooling). The s3api.AuditMiddleware publishes
// each persisted row; subscribers (the SSE endpoint at
// /admin/v1/audit/stream) receive a non-blocking, server-side filtered copy.
//
// The broadcaster never blocks the audit-write hot path: a slow subscriber
// drops frames and a once-per-minute WARN line records the loss. Cancelling
// the subscriber context removes it from the fan-out.
package auditstream

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/danchupin/strata/internal/meta"
)

// SubscriberBufferSize is the per-subscriber channel buffer. Frames that
// arrive while the subscriber is full are dropped.
const SubscriberBufferSize = 256

// Filter selects which audit rows a subscriber receives. Empty fields match
// everything; non-empty fields require an exact (case-insensitive for Action)
// match.
type Filter struct {
	Action    string
	Principal string
	Bucket    string
}

func (f Filter) match(e *meta.AuditEvent) bool {
	if f.Action != "" && !strings.EqualFold(f.Action, e.Action) {
		return false
	}
	if f.Principal != "" && f.Principal != e.Principal {
		return false
	}
	if f.Bucket != "" && f.Bucket != e.Bucket {
		return false
	}
	return true
}

// MetricsSink is the optional Prometheus adapter receiving subscriber-count
// updates. nil disables metric updates.
type MetricsSink interface {
	SetSubscribers(n int)
}

type subscriber struct {
	ch     chan *meta.AuditEvent
	filter Filter
	drops  uint64
}

// Broadcaster is the fan-out hub. Zero value is not usable — call New.
type Broadcaster struct {
	mu     sync.RWMutex
	subs   map[*subscriber]struct{}
	logger *slog.Logger
	sink   MetricsSink

	dropMu        sync.Mutex
	lastDropLog   time.Time
	dropsInWindow uint64
}

// New returns a Broadcaster. logger may be nil (falls back to slog.Default);
// sink may be nil (no metric updates).
func New(logger *slog.Logger, sink MetricsSink) *Broadcaster {
	if logger == nil {
		logger = slog.Default()
	}
	return &Broadcaster{
		subs:   make(map[*subscriber]struct{}),
		logger: logger,
		sink:   sink,
	}
}

// Subscribe registers a subscriber and returns the receive channel. The
// channel is never closed by the broadcaster — callers select on
// ctx.Done() to detect shutdown. Cancelling ctx removes the subscriber from
// the fan-out.
func (b *Broadcaster) Subscribe(ctx context.Context, f Filter) <-chan *meta.AuditEvent {
	sub := &subscriber{
		ch:     make(chan *meta.AuditEvent, SubscriberBufferSize),
		filter: f,
	}
	b.mu.Lock()
	b.subs[sub] = struct{}{}
	n := len(b.subs)
	b.mu.Unlock()
	if b.sink != nil {
		b.sink.SetSubscribers(n)
	}
	go func() {
		<-ctx.Done()
		b.unsubscribe(sub)
	}()
	return sub.ch
}

func (b *Broadcaster) unsubscribe(sub *subscriber) {
	b.mu.Lock()
	if _, ok := b.subs[sub]; !ok {
		b.mu.Unlock()
		return
	}
	delete(b.subs, sub)
	n := len(b.subs)
	b.mu.Unlock()
	if b.sink != nil {
		b.sink.SetSubscribers(n)
	}
}

// Publish fans out e to every matching subscriber. Non-blocking: if a
// subscriber's buffer is full the frame is dropped and a slow-subscriber WARN
// is rate-limited to once per minute.
func (b *Broadcaster) Publish(e *meta.AuditEvent) {
	if e == nil {
		return
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	for s := range b.subs {
		if !s.filter.match(e) {
			continue
		}
		select {
		case s.ch <- e:
		default:
			atomic.AddUint64(&s.drops, 1)
			b.recordDrop()
		}
	}
}

// SubscriberCount returns the current subscriber count. Test helper.
func (b *Broadcaster) SubscriberCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subs)
}

func (b *Broadcaster) recordDrop() {
	b.dropMu.Lock()
	b.dropsInWindow++
	emit := false
	n := b.dropsInWindow
	if time.Since(b.lastDropLog) >= time.Minute {
		emit = true
		b.lastDropLog = time.Now()
		b.dropsInWindow = 0
	}
	b.dropMu.Unlock()
	if emit {
		b.logger.Warn("audit stream slow subscriber dropped frames", "drops_in_window", n)
	}
}
