package ringbuf

import (
	"context"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

type fakeMetrics struct {
	mu      sync.Mutex
	traces  int
	evicted int
}

func (f *fakeMetrics) SetTraces(n int) {
	f.mu.Lock()
	f.traces = n
	f.mu.Unlock()
}
func (f *fakeMetrics) IncEvicted() {
	f.mu.Lock()
	f.evicted++
	f.mu.Unlock()
}

func (f *fakeMetrics) snapshot() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.traces, f.evicted
}

// fakeSpan returns a tracetest.SpanStub-backed read-only span suitable for
// OnEnd. We round-trip via tracetest.SpanStubs.Snapshots() because the SDK's
// ReadOnlySpan interface is unexported.
func fakeSpan(traceID trace.TraceID, spanID trace.SpanID, parent trace.SpanID, name, requestID string) tracetest.SpanStub {
	stub := tracetest.SpanStub{
		Name: name,
		SpanContext: trace.NewSpanContext(trace.SpanContextConfig{
			TraceID: traceID,
			SpanID:  spanID,
		}),
	}
	if parent.IsValid() {
		stub.Parent = trace.NewSpanContext(trace.SpanContextConfig{
			TraceID: traceID,
			SpanID:  parent,
		})
	}
	if requestID != "" {
		stub.Attributes = []attribute.KeyValue{attribute.String(AttributeKeyRequestID, requestID)}
	}
	return stub
}

func tid(b byte) trace.TraceID {
	var t trace.TraceID
	t[0] = b
	t[15] = 1
	return t
}

func sid(b byte) trace.SpanID {
	var s trace.SpanID
	s[0] = b
	s[7] = 1
	return s
}

func ingest(t *testing.T, rb *RingBuffer, stub tracetest.SpanStub) {
	t.Helper()
	stubs := tracetest.SpanStubs{stub}
	for _, s := range stubs.Snapshots() {
		rb.OnEnd(s)
	}
}

func TestOnEndIndexesByTraceAndRequestID(t *testing.T) {
	m := &fakeMetrics{}
	rb := New(WithMetrics(m))

	ingest(t, rb, fakeSpan(tid(1), sid(0xa), trace.SpanID{}, "PUT /bkt/key", "req-1"))
	ingest(t, rb, fakeSpan(tid(1), sid(0xb), sid(0xa), "meta.cassandra.objects.INSERT", ""))

	got, ok := rb.GetByRequestID("req-1")
	if !ok {
		t.Fatal("trace not found by request id")
	}
	if got.TraceID != tid(1).String() {
		t.Errorf("trace id = %q want %q", got.TraceID, tid(1).String())
	}
	if got.RequestID != "req-1" {
		t.Errorf("request id = %q want req-1", got.RequestID)
	}
	if len(got.Spans) != 2 {
		t.Fatalf("spans=%d want 2: %+v", len(got.Spans), got.Spans)
	}
	if got.Root != sid(0xa).String() {
		t.Errorf("root = %q want %q", got.Root, sid(0xa).String())
	}

	// Direct trace-id lookup also works.
	if _, ok := rb.GetByTraceID(tid(1).String()); !ok {
		t.Errorf("GetByTraceID returned ok=false for known trace")
	}
	if _, ok := rb.GetByTraceID("zz"); ok {
		t.Errorf("GetByTraceID returned ok=true for invalid hex")
	}

	// Metrics gauge updated to 1.
	if traces, _ := m.snapshot(); traces != 1 {
		t.Errorf("metrics traces = %d want 1", traces)
	}
}

func TestUnknownRequestIDIsMiss(t *testing.T) {
	rb := New()
	if _, ok := rb.GetByRequestID("nope"); ok {
		t.Errorf("expected miss")
	}
}

func TestBytesBudgetLRUEvictsOldest(t *testing.T) {
	m := &fakeMetrics{}
	// Tiny budget — every span comfortably exceeds it. We size a span at
	// roughly perSpanOverhead + len(name) + a little, so capBytes=200 fits
	// at most ~1 span at a time given the trailing tail's overhead.
	rb := New(WithBytes(200), WithMetrics(m))

	for i := range 5 {
		stub := fakeSpan(tid(byte(i+1)), sid(0xa), trace.SpanID{}, "PUT /bkt/keyaaaaaaaaaaaaaaaaaaaaaa", "")
		ingest(t, rb, stub)
	}

	// Oldest traces (1..3 typically) should have been evicted; only the
	// most recent should remain.
	if rb.TraceCount() == 0 {
		t.Fatal("expected at least one trace retained")
	}
	if rb.TraceCount() >= 5 {
		t.Errorf("expected eviction; trace count = %d", rb.TraceCount())
	}
	if _, evicted := m.snapshot(); evicted == 0 {
		t.Errorf("expected at least one evicted counter increment")
	}

	// The newest trace must still be present.
	newestTID := tid(5)
	if _, ok := rb.GetByTraceID(newestTID.String()); !ok {
		t.Errorf("newest trace evicted; ringbuf misordering?")
	}

	// And the oldest must be gone.
	oldestTID := tid(1)
	if _, ok := rb.GetByTraceID(oldestTID.String()); ok {
		t.Errorf("oldest trace not evicted")
	}
}

func TestPerTraceSpanCapDropsExcessOnce(t *testing.T) {
	rb := New(WithSpanCap(2))

	for i := range 5 {
		stub := fakeSpan(tid(1), sid(byte(i+1)), trace.SpanID{}, "child", "")
		ingest(t, rb, stub)
	}
	got, ok := rb.GetByTraceID(tid(1).String())
	if !ok {
		t.Fatal("trace missing")
	}
	if len(got.Spans) != 2 {
		t.Errorf("retained spans=%d want 2 (cap honoured)", len(got.Spans))
	}
}

func TestInvalidSpanContextIsIgnored(t *testing.T) {
	rb := New()
	// trace.SpanID{} + trace.TraceID{} are invalid. Build via SpanContextConfig.
	stubs := tracetest.SpanStubs{tracetest.SpanStub{Name: "no-context"}}
	for _, s := range stubs.Snapshots() {
		rb.OnEnd(s)
	}
	if rb.TraceCount() != 0 {
		t.Errorf("expected invalid span ignored")
	}
}

func TestSpanProcessorContractIsNoopExceptOnEnd(t *testing.T) {
	rb := New()
	rb.OnStart(context.Background(), nil)
	if err := rb.Shutdown(context.Background()); err != nil {
		t.Errorf("shutdown: %v", err)
	}
	if err := rb.ForceFlush(context.Background()); err != nil {
		t.Errorf("flush: %v", err)
	}
}

func TestAttributesFlow(t *testing.T) {
	rb := New()
	stub := fakeSpan(tid(1), sid(0xa), trace.SpanID{}, "PUT /bkt/key", "req-x")
	stub.Attributes = append(stub.Attributes,
		attribute.String("http.method", "PUT"),
		attribute.Int("http.status_code", 200),
	)
	ingest(t, rb, stub)

	got, _ := rb.GetByRequestID("req-x")
	if got.Spans[0].Attributes["http.method"] != "PUT" {
		t.Errorf("http.method missing or wrong: %v", got.Spans[0].Attributes)
	}
	if v, ok := got.Spans[0].Attributes["http.status_code"].(int64); !ok || v != 200 {
		t.Errorf("http.status_code missing/typed wrong: %v", got.Spans[0].Attributes)
	}
}

func TestRootSpanIDPicksParentless(t *testing.T) {
	rb := New()
	ingest(t, rb, fakeSpan(tid(1), sid(0xb), sid(0xa), "child", ""))
	ingest(t, rb, fakeSpan(tid(1), sid(0xa), trace.SpanID{}, "root", "req-r"))
	got, _ := rb.GetByRequestID("req-r")
	if got.Root != sid(0xa).String() {
		t.Errorf("root = %q want %q", got.Root, sid(0xa).String())
	}
}

// Sanity-check that the JSON-friendly Trace shape carries everything the
// admin handler advertises.
func TestTraceShapeContainsExpectedFields(t *testing.T) {
	rb := New()
	stub := fakeSpan(tid(2), sid(0xc), trace.SpanID{}, "GET /bkt", "req-y")
	stub.Attributes = append(stub.Attributes, attribute.String("http.target", "/bkt"))
	ingest(t, rb, stub)

	got, _ := rb.GetByRequestID("req-y")
	if got.TraceID == "" || got.RequestID == "" || got.Root == "" {
		t.Errorf("Trace missing top-level fields: %+v", got)
	}
	if got.Spans[0].StartNS == 0 && got.Spans[0].EndNS == 0 {
		// SpanStubs without Start/End times will surface zero — that's fine
		// (the test uses default-constructed stubs); we only assert shape.
		t.Log("note: SpanStub had zero start/end (default-constructed)")
	}
	if got.Spans[0].Status == "" {
		t.Errorf("status string empty")
	}
	if !strings.HasPrefix(got.Spans[0].Name, "GET ") {
		t.Errorf("name lost: %q", got.Spans[0].Name)
	}
}
