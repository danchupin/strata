// Package ringbuf is the in-process trace ring buffer (US-005 — Phase 3
// debug tooling). It implements sdktrace.SpanProcessor and retains the most
// recent traces under a configurable bytes budget so the
// /admin/v1/diagnostics/trace/{requestID} endpoint can serve a waterfall view
// without a Jaeger / Tempo deployment.
//
// Spans are grouped by trace_id with a secondary index request_id → trace_id.
// The SDK keys traces by trace_id; operators paste the X-Request-Id header
// the gateway returns, so both lookups must work. A bytes-budgeted LRU
// (oldest trace evicted first) bounds memory; a per-trace span cap drops
// further spans on a runaway trace with one WARN per affected trace.
package ringbuf

import (
	"container/list"
	"context"
	"log/slog"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

const (
	// DefaultBytesBudget is the in-process retention budget when
	// STRATA_OTEL_RINGBUF_BYTES is unset (4 MiB).
	DefaultBytesBudget = 4 << 20
	// PerTraceSpanCap caps the number of spans retained per trace. Further
	// spans are dropped with one WARN per trace.
	PerTraceSpanCap = 256
	// AttributeKeyRequestID is the span attribute carrying the Strata
	// request id; the HTTP middleware sets it on the root span.
	AttributeKeyRequestID = "request_id"

	// perSpanOverhead is the bookkeeping cost we charge each span on top of
	// its name + attribute payload. Tuned so a typical span (~1 short name
	// plus ~5 small attributes) lands near 200 bytes.
	perSpanOverhead = 64
)

// MetricsSink lets the binary plug Prometheus into the ring buffer without
// pulling prometheus into this package. Nil falls back to a no-op.
type MetricsSink interface {
	SetTraces(n int)
	IncEvicted()
}

type noopMetrics struct{}

func (noopMetrics) SetTraces(int) {}
func (noopMetrics) IncEvicted()   {}

// Span is the wire shape returned to the operator-facing trace endpoint.
// Field names match the JSON contract spelled out in the US-005 AC.
type Span struct {
	SpanID     string         `json:"span_id"`
	Parent     string         `json:"parent,omitempty"`
	Name       string         `json:"name"`
	StartNS    int64          `json:"start_ns"`
	EndNS      int64          `json:"end_ns"`
	Status     string         `json:"status"`
	Attributes map[string]any `json:"attributes,omitempty"`
}

// Trace is the wire shape returned by the admin handler.
type Trace struct {
	TraceID   string `json:"trace_id"`
	RequestID string `json:"request_id,omitempty"`
	Root      string `json:"root,omitempty"`
	Spans     []Span `json:"spans"`
}

// RingBuffer retains traces in process. Implements sdktrace.SpanProcessor.
type RingBuffer struct {
	capBytes int
	spanCap  int
	metrics  MetricsSink
	logger   *slog.Logger

	mu     sync.Mutex
	bytes  int
	traces map[trace.TraceID]*entry
	byReq  map[string]trace.TraceID
	lru    *list.List // *entry; back = oldest, front = most recent
}

type entry struct {
	traceID   trace.TraceID
	requestID string
	spans     []Span
	bytes     int
	elem      *list.Element
	over      bool
}

// Option tunes a fresh RingBuffer.
type Option func(*RingBuffer)

// WithBytes overrides the retention budget. Non-positive values are ignored
// so callers can pass an env-derived value without an extra branch.
func WithBytes(n int) Option {
	return func(r *RingBuffer) {
		if n > 0 {
			r.capBytes = n
		}
	}
}

// WithSpanCap overrides the per-trace span cap. Tests use this to hit the
// drop-and-warn path without storing 256 spans.
func WithSpanCap(n int) Option {
	return func(r *RingBuffer) {
		if n > 0 {
			r.spanCap = n
		}
	}
}

// WithMetrics installs a Prometheus adapter; nil leaves the no-op sink.
func WithMetrics(m MetricsSink) Option {
	return func(r *RingBuffer) {
		if m != nil {
			r.metrics = m
		}
	}
}

// WithLogger overrides the slog.Logger used for once-per-trace WARNs.
func WithLogger(l *slog.Logger) Option {
	return func(r *RingBuffer) {
		if l != nil {
			r.logger = l
		}
	}
}

// New builds a ring buffer ready to plug into a TracerProvider.
func New(opts ...Option) *RingBuffer {
	rb := &RingBuffer{
		capBytes: DefaultBytesBudget,
		spanCap:  PerTraceSpanCap,
		metrics:  noopMetrics{},
		logger:   slog.Default(),
		traces:   make(map[trace.TraceID]*entry),
		byReq:    make(map[string]trace.TraceID),
		lru:      list.New(),
	}
	for _, o := range opts {
		o(rb)
	}
	return rb
}

// OnStart is a no-op; we only retain finished spans.
func (r *RingBuffer) OnStart(_ context.Context, _ sdktrace.ReadWriteSpan) {}

// Shutdown is a no-op. The ring buffer holds in-process state only.
func (r *RingBuffer) Shutdown(_ context.Context) error { return nil }

// ForceFlush is a no-op; spans land synchronously in OnEnd.
func (r *RingBuffer) ForceFlush(_ context.Context) error { return nil }

// OnEnd records the span. Drops malformed (invalid SpanContext) spans.
func (r *RingBuffer) OnEnd(s sdktrace.ReadOnlySpan) {
	sc := s.SpanContext()
	if !sc.IsValid() {
		return
	}
	tid := sc.TraceID()
	sp := toSpan(s)
	reqID := deriveRequestID(s)
	r.record(tid, sp, reqID)
}

func (r *RingBuffer) record(tid trace.TraceID, sp Span, reqID string) {
	size := estimateSize(sp)

	r.mu.Lock()
	defer r.mu.Unlock()

	e, ok := r.traces[tid]
	if !ok {
		e = &entry{traceID: tid, spans: make([]Span, 0, 4)}
		e.elem = r.lru.PushFront(e)
		r.traces[tid] = e
	} else {
		r.lru.MoveToFront(e.elem)
	}

	if reqID != "" && e.requestID == "" {
		e.requestID = reqID
		r.byReq[reqID] = tid
	}

	if len(e.spans) >= r.spanCap {
		if !e.over {
			e.over = true
			r.logger.Warn("otel ringbuf: per-trace span cap exceeded; dropping further spans",
				"trace_id", tid.String(), "cap", r.spanCap)
		}
		return
	}

	e.spans = append(e.spans, sp)
	e.bytes += size
	r.bytes += size

	r.evict()
	r.metrics.SetTraces(len(r.traces))
}

func (r *RingBuffer) evict() {
	for r.bytes > r.capBytes {
		back := r.lru.Back()
		if back == nil {
			return
		}
		e := back.Value.(*entry)
		r.lru.Remove(back)
		delete(r.traces, e.traceID)
		if e.requestID != "" {
			if cur, ok := r.byReq[e.requestID]; ok && cur == e.traceID {
				delete(r.byReq, e.requestID)
			}
		}
		r.bytes -= e.bytes
		if r.bytes < 0 {
			r.bytes = 0
		}
		r.metrics.IncEvicted()
	}
}

// GetByRequestID returns the trace doc keyed by the X-Request-Id value the
// gateway returned. ok=false when the id is unknown (typical when the trace
// has aged out of the ring).
func (r *RingBuffer) GetByRequestID(reqID string) (Trace, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	tid, ok := r.byReq[reqID]
	if !ok {
		return Trace{}, false
	}
	e, ok := r.traces[tid]
	if !ok {
		return Trace{}, false
	}
	return r.snapshot(e), true
}

// GetByTraceID returns the trace doc keyed by raw OTel trace id. Used when
// the operator paste the trace id directly (debug-only path).
func (r *RingBuffer) GetByTraceID(tidStr string) (Trace, bool) {
	tid, err := trace.TraceIDFromHex(tidStr)
	if err != nil {
		return Trace{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.traces[tid]
	if !ok {
		return Trace{}, false
	}
	return r.snapshot(e), true
}

func (r *RingBuffer) snapshot(e *entry) Trace {
	out := make([]Span, len(e.spans))
	copy(out, e.spans)
	return Trace{
		TraceID:   e.traceID.String(),
		RequestID: e.requestID,
		Root:      rootSpanID(out),
		Spans:     out,
	}
}

// TraceCount returns the number of retained traces. Used by tests + the
// metrics gauge.
func (r *RingBuffer) TraceCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.traces)
}

// Bytes returns the current bytes-used count. Test-only.
func (r *RingBuffer) Bytes() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.bytes
}

func toSpan(s sdktrace.ReadOnlySpan) Span {
	parent := ""
	if pc := s.Parent(); pc.IsValid() {
		parent = pc.SpanID().String()
	}
	return Span{
		SpanID:     s.SpanContext().SpanID().String(),
		Parent:     parent,
		Name:       s.Name(),
		StartNS:    s.StartTime().UnixNano(),
		EndNS:      s.EndTime().UnixNano(),
		Status:     statusString(s.Status().Code),
		Attributes: attrsToMap(s.Attributes()),
	}
}

func deriveRequestID(s sdktrace.ReadOnlySpan) string {
	for _, kv := range s.Attributes() {
		if string(kv.Key) == AttributeKeyRequestID && kv.Value.Type() == attribute.STRING {
			return kv.Value.AsString()
		}
	}
	return ""
}

func statusString(c codes.Code) string {
	switch c {
	case codes.Ok:
		return "OK"
	case codes.Error:
		return "Error"
	default:
		return "Unset"
	}
}

func attrsToMap(a []attribute.KeyValue) map[string]any {
	if len(a) == 0 {
		return nil
	}
	m := make(map[string]any, len(a))
	for _, kv := range a {
		m[string(kv.Key)] = attrValue(kv.Value)
	}
	return m
}

func attrValue(v attribute.Value) any {
	switch v.Type() {
	case attribute.STRING:
		return v.AsString()
	case attribute.BOOL:
		return v.AsBool()
	case attribute.INT64:
		return v.AsInt64()
	case attribute.FLOAT64:
		return v.AsFloat64()
	case attribute.STRINGSLICE:
		return v.AsStringSlice()
	case attribute.BOOLSLICE:
		return v.AsBoolSlice()
	case attribute.INT64SLICE:
		return v.AsInt64Slice()
	case attribute.FLOAT64SLICE:
		return v.AsFloat64Slice()
	default:
		return v.Emit()
	}
}

func estimateSize(s Span) int {
	n := perSpanOverhead + len(s.Name) + len(s.Status) + len(s.SpanID) + len(s.Parent)
	for k, v := range s.Attributes {
		n += len(k)
		switch x := v.(type) {
		case string:
			n += len(x)
		case []string:
			for _, e := range x {
				n += len(e)
			}
		default:
			n += 8
		}
	}
	return n
}

func rootSpanID(spans []Span) string {
	for _, s := range spans {
		if s.Parent == "" {
			return s.SpanID
		}
	}
	return ""
}
