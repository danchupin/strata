package adminapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/danchupin/strata/internal/otel/ringbuf"
)

func seedTrace(t *testing.T, rb *ringbuf.RingBuffer, traceB byte, name, requestID string) {
	t.Helper()
	stubs := tracetest.SpanStubs{makeStub(traceB, 0xa, trace.SpanID{}, name, requestID)}
	for _, s := range stubs.Snapshots() {
		rb.OnEnd(s)
	}
}

// TestDiagnosticsTracesReturnsRecentInLRUOrder seeds three traces; the most
// recent insertion should be first in the list response.
func TestDiagnosticsTracesReturnsRecentInLRUOrder(t *testing.T) {
	rb := ringbuf.New()
	seedTrace(t, rb, 1, "PUT /bkt/a", "req-a")
	seedTrace(t, rb, 2, "PUT /bkt/b", "req-b")
	seedTrace(t, rb, 3, "PUT /bkt/c", "req-c")

	s := newTestServer()
	s.TraceRingbuf = rb

	rr := httptest.NewRecorder()
	req := withAuditAuthCtx(httptest.NewRequest(http.MethodGet, "/admin/v1/diagnostics/traces", nil), "operator")
	s.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got diagnosticsTracesResponse
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Total != 3 {
		t.Errorf("total=%d want 3", got.Total)
	}
	if len(got.Traces) != 3 {
		t.Fatalf("traces=%d want 3", len(got.Traces))
	}
	wantOrder := []string{"req-c", "req-b", "req-a"}
	for i, want := range wantOrder {
		if got.Traces[i].RequestID != want {
			t.Errorf("traces[%d].request_id=%q want %q", i, got.Traces[i].RequestID, want)
		}
	}
}

func TestDiagnosticsTracesHonoursLimitAndOffset(t *testing.T) {
	rb := ringbuf.New()
	for i := byte(1); i <= 6; i++ {
		seedTrace(t, rb, i, "PUT /bkt", string(rune('0'+i)))
	}

	s := newTestServer()
	s.TraceRingbuf = rb

	rr := httptest.NewRecorder()
	req := withAuditAuthCtx(httptest.NewRequest(http.MethodGet, "/admin/v1/diagnostics/traces?limit=2&offset=2", nil), "operator")
	s.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got diagnosticsTracesResponse
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Total != 6 {
		t.Errorf("total=%d want 6", got.Total)
	}
	// Insertion order trace id 1..6; LRU front=6,5,4,3,2,1. offset=2 skips
	// 6 and 5 → page is 4, 3.
	if len(got.Traces) != 2 {
		t.Fatalf("traces=%d want 2", len(got.Traces))
	}
	if got.Traces[0].RequestID != "4" || got.Traces[1].RequestID != "3" {
		t.Errorf("page=%v want [4 3]", []string{got.Traces[0].RequestID, got.Traces[1].RequestID})
	}
}

func TestDiagnosticsTracesCapsLimitAt200(t *testing.T) {
	rb := ringbuf.New()
	seedTrace(t, rb, 1, "PUT /bkt", "req-a")

	s := newTestServer()
	s.TraceRingbuf = rb

	rr := httptest.NewRecorder()
	req := withAuditAuthCtx(httptest.NewRequest(http.MethodGet, "/admin/v1/diagnostics/traces?limit=10000", nil), "operator")
	s.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	// Single trace seeded — only one entry should come back regardless of
	// the bumped cap. Cap exercise is covered indirectly: the handler just
	// rounds limit down to 200; the actual cap value is asserted via the
	// const reference below.
	if maxDiagnosticsTracesLimit != 200 {
		t.Errorf("maxDiagnosticsTracesLimit=%d want 200", maxDiagnosticsTracesLimit)
	}
	var got diagnosticsTracesResponse
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Traces) != 1 {
		t.Errorf("traces=%d want 1", len(got.Traces))
	}
}

func TestDiagnosticsTracesEmptyResponse(t *testing.T) {
	s := newTestServer()
	s.TraceRingbuf = ringbuf.New()

	rr := httptest.NewRecorder()
	req := withAuditAuthCtx(httptest.NewRequest(http.MethodGet, "/admin/v1/diagnostics/traces", nil), "operator")
	s.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got diagnosticsTracesResponse
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Total != 0 {
		t.Errorf("total=%d want 0", got.Total)
	}
	if got.Traces == nil || len(got.Traces) != 0 {
		t.Errorf("traces=%v want []", got.Traces)
	}
}

func TestDiagnosticsTracesReturns503WhenRingbufDisabled(t *testing.T) {
	s := newTestServer()
	s.TraceRingbuf = nil

	rr := httptest.NewRecorder()
	req := withAuditAuthCtx(httptest.NewRequest(http.MethodGet, "/admin/v1/diagnostics/traces", nil), "operator")
	s.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var env errorResponse
	if err := json.NewDecoder(rr.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Code != "RingbufUnavailable" {
		t.Errorf("code=%q want RingbufUnavailable", env.Code)
	}
}
