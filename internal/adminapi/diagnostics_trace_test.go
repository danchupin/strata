package adminapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/danchupin/strata/internal/otel/ringbuf"
)

func makeStub(traceB byte, spanB byte, parent trace.SpanID, name, requestID string) tracetest.SpanStub {
	var tid trace.TraceID
	tid[0] = traceB
	tid[15] = 1
	var sid trace.SpanID
	sid[0] = spanB
	sid[7] = 1
	stub := tracetest.SpanStub{
		Name: name,
		SpanContext: trace.NewSpanContext(trace.SpanContextConfig{
			TraceID: tid,
			SpanID:  sid,
		}),
	}
	if parent.IsValid() {
		stub.Parent = trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: parent})
	}
	if requestID != "" {
		stub.Attributes = append(stub.Attributes,
			attribute.String(ringbuf.AttributeKeyRequestID, requestID),
		)
	}
	return stub
}

// TestDiagnosticsTraceReturnsKnownTrace seeds a ring buffer with a span
// carrying request_id="req-1" and asserts the handler returns 200 + the
// trace doc.
func TestDiagnosticsTraceReturnsKnownTrace(t *testing.T) {
	rb := ringbuf.New()
	stubs := tracetest.SpanStubs{makeStub(1, 0xa, trace.SpanID{}, "PUT /bkt/key", "req-1")}
	for _, s := range stubs.Snapshots() {
		rb.OnEnd(s)
	}

	s := newTestServer()
	s.TraceRingbuf = rb

	rr := httptest.NewRecorder()
	req := withAuditAuthCtx(httptest.NewRequest(http.MethodGet, "/admin/v1/diagnostics/trace/req-1", nil), "operator")
	s.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got ringbuf.Trace
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.RequestID != "req-1" {
		t.Errorf("request_id = %q want req-1", got.RequestID)
	}
	if len(got.Spans) != 1 {
		t.Errorf("spans=%d want 1", len(got.Spans))
	}
	if got.Spans[0].Name != "PUT /bkt/key" {
		t.Errorf("name=%q", got.Spans[0].Name)
	}
}

func TestDiagnosticsTraceReturns404OnUnknown(t *testing.T) {
	s := newTestServer()
	s.TraceRingbuf = ringbuf.New()

	rr := httptest.NewRecorder()
	req := withAuditAuthCtx(httptest.NewRequest(http.MethodGet, "/admin/v1/diagnostics/trace/missing", nil), "operator")
	s.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var env errorResponse
	if err := json.NewDecoder(rr.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Code != "NotFound" {
		t.Errorf("code=%q", env.Code)
	}
}

func TestDiagnosticsTraceReturns503WhenRingbufDisabled(t *testing.T) {
	s := newTestServer()
	s.TraceRingbuf = nil

	rr := httptest.NewRecorder()
	req := withAuditAuthCtx(httptest.NewRequest(http.MethodGet, "/admin/v1/diagnostics/trace/anything", nil), "operator")
	s.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var env errorResponse
	if err := json.NewDecoder(rr.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Code != "RingbufUnavailable" {
		t.Errorf("code=%q", env.Code)
	}
}

// TestDiagnosticsTraceFallsBackToTraceID covers the operator-paste-trace-id
// path: when request_id is unknown but the value parses as a 32-hex trace id
// we still resolve.
func TestDiagnosticsTraceFallsBackToTraceID(t *testing.T) {
	rb := ringbuf.New()
	stubs := tracetest.SpanStubs{makeStub(2, 0xb, trace.SpanID{}, "GET /bkt", "")}
	for _, s := range stubs.Snapshots() {
		rb.OnEnd(s)
	}
	var tid trace.TraceID
	tid[0] = 2
	tid[15] = 1

	s := newTestServer()
	s.TraceRingbuf = rb

	rr := httptest.NewRecorder()
	req := withAuditAuthCtx(httptest.NewRequest(http.MethodGet, "/admin/v1/diagnostics/trace/"+tid.String(), nil), "operator")
	s.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got ringbuf.Trace
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.TraceID != tid.String() {
		t.Errorf("trace_id=%q", got.TraceID)
	}
}

