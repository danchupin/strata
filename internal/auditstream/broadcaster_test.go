package auditstream

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/danchupin/strata/internal/meta"
)

func TestBroadcasterFanOutMultipleSubscribers(t *testing.T) {
	b := New(nil, nil)
	ctx := t.Context()

	c1 := b.Subscribe(ctx, Filter{})
	c2 := b.Subscribe(ctx, Filter{})

	ev := &meta.AuditEvent{Action: "PutObject", Bucket: "b1"}
	b.Publish(ev)

	for i, ch := range []<-chan *meta.AuditEvent{c1, c2} {
		select {
		case got := <-ch:
			if got.Action != "PutObject" {
				t.Errorf("sub %d: action=%q", i, got.Action)
			}
		case <-time.After(time.Second):
			t.Fatalf("sub %d: no frame", i)
		}
	}
}

func TestBroadcasterFilterServerSide(t *testing.T) {
	b := New(nil, nil)
	ctx := t.Context()

	matchCh := b.Subscribe(ctx, Filter{Action: "DeleteObject", Bucket: "b1"})
	skipCh := b.Subscribe(ctx, Filter{Action: "PutObject"})

	b.Publish(&meta.AuditEvent{Action: "DeleteObject", Bucket: "b1"})

	select {
	case got := <-matchCh:
		if got.Action != "DeleteObject" {
			t.Errorf("match: action=%q", got.Action)
		}
	case <-time.After(time.Second):
		t.Fatalf("match: no frame")
	}

	select {
	case got := <-skipCh:
		t.Fatalf("skip: unexpected frame %+v", got)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestBroadcasterFilterCaseInsensitiveAction(t *testing.T) {
	b := New(nil, nil)
	ctx := t.Context()

	ch := b.Subscribe(ctx, Filter{Action: "putobject"})
	b.Publish(&meta.AuditEvent{Action: "PutObject"})

	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatalf("no frame on case-insensitive match")
	}
}

func TestBroadcasterSlowSubscriberDropsFrames(t *testing.T) {
	b := New(nil, nil)
	ctx := t.Context()

	// Subscribe but never read — buffer fills then drops.
	_ = b.Subscribe(ctx, Filter{})

	for range SubscriberBufferSize + 50 {
		b.Publish(&meta.AuditEvent{Action: "PutObject"})
	}

	// Publish must not block, hence the test reaching here is the assertion.
	// Inspect the unexported subscriber drop counter via a read-only walk.
	b.mu.RLock()
	defer b.mu.RUnlock()
	if len(b.subs) != 1 {
		t.Fatalf("subs=%d want 1", len(b.subs))
	}
	for s := range b.subs {
		if s.drops == 0 {
			t.Errorf("expected non-zero drop count")
		}
	}
}

func TestBroadcasterPublishNeverBlocksAuditPath(t *testing.T) {
	b := New(nil, nil)
	ctx := t.Context()

	for range 10 {
		_ = b.Subscribe(ctx, Filter{})
	}

	done := make(chan struct{})
	go func() {
		for range SubscriberBufferSize * 4 {
			b.Publish(&meta.AuditEvent{Action: "PutObject"})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("Publish blocked under slow-subscriber load")
	}
}

func TestBroadcasterCancelRemovesSubscriber(t *testing.T) {
	b := New(nil, nil)
	ctx, cancel := context.WithCancel(context.Background())

	_ = b.Subscribe(ctx, Filter{})
	if b.SubscriberCount() != 1 {
		t.Fatalf("subs=%d want 1", b.SubscriberCount())
	}
	cancel()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if b.SubscriberCount() == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("subscriber not removed after ctx cancel; subs=%d", b.SubscriberCount())
}

func TestBroadcasterMetricsSinkUpdatesOnSubAndUnsub(t *testing.T) {
	sink := &fakeSink{}
	b := New(nil, sink)

	ctx1, cancel1 := context.WithCancel(context.Background())
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()

	_ = b.Subscribe(ctx1, Filter{})
	_ = b.Subscribe(ctx2, Filter{})

	if got := sink.last(); got != 2 {
		t.Errorf("sink last after 2 subs = %d want 2", got)
	}

	cancel1()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if sink.last() == 1 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("sink last after cancel = %d want 1", sink.last())
}

type fakeSink struct {
	mu sync.Mutex
	n  int
	hi int
}

func (f *fakeSink) SetSubscribers(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n = n
	if n > f.hi {
		f.hi = n
	}
}

func (f *fakeSink) last() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.n
}
