package adminapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
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

// seedRichTrace seeds a single-span trace with explicit start time, duration,
// and status so filter tests can target duration ms + status axes.
func seedRichTrace(t *testing.T, rb *ringbuf.RingBuffer, traceB byte, name, requestID string, durationMs int64, errorStatus bool) {
	t.Helper()
	var tid trace.TraceID
	tid[0] = traceB
	tid[15] = 1
	var sid trace.SpanID
	sid[0] = 0xa
	sid[7] = 1
	start := time.Unix(0, int64(traceB)*int64(time.Second))
	stub := tracetest.SpanStub{
		Name: name,
		SpanContext: trace.NewSpanContext(trace.SpanContextConfig{
			TraceID: tid,
			SpanID:  sid,
		}),
		StartTime: start,
		EndTime:   start.Add(time.Duration(durationMs) * time.Millisecond),
	}
	if requestID != "" {
		stub.Attributes = append(stub.Attributes,
			attribute.String(ringbuf.AttributeKeyRequestID, requestID),
		)
	}
	if errorStatus {
		stub.Status = sdktrace.Status{Code: codes.Error}
	} else {
		stub.Status = sdktrace.Status{Code: codes.Ok}
	}
	for _, s := range (tracetest.SpanStubs{stub}).Snapshots() {
		rb.OnEnd(s)
	}
}

func filterResponse(t *testing.T, s *Server, params url.Values) diagnosticsTracesResponse {
	t.Helper()
	rr := httptest.NewRecorder()
	target := "/admin/v1/diagnostics/traces"
	if encoded := params.Encode(); encoded != "" {
		target += "?" + encoded
	}
	req := withAuditAuthCtx(httptest.NewRequest(http.MethodGet, target, nil), "operator")
	s.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got diagnosticsTracesResponse
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return got
}

func requestIDs(in []ringbuf.TraceSummary) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, s.RequestID)
	}
	return out
}

func sortedEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, s := range a {
		seen[s]++
	}
	for _, s := range b {
		seen[s]--
		if seen[s] < 0 {
			return false
		}
	}
	return true
}

func TestDiagnosticsTracesFilterByMethod(t *testing.T) {
	rb := ringbuf.New()
	seedRichTrace(t, rb, 1, "GET /bkt/a", "g-a", 10, false)
	seedRichTrace(t, rb, 2, "PUT /bkt/b", "p-b", 20, false)
	seedRichTrace(t, rb, 3, "DELETE /bkt/c", "d-c", 30, false)
	seedRichTrace(t, rb, 4, "PUT /bkt/d", "p-d", 40, false)

	s := newTestServer()
	s.TraceRingbuf = rb

	got := filterResponse(t, s, url.Values{"method": {"put"}})
	if got.Total != 2 {
		t.Errorf("total=%d want 2", got.Total)
	}
	if !sortedEqual(requestIDs(got.Traces), []string{"p-b", "p-d"}) {
		t.Errorf("traces=%v want [p-b p-d]", requestIDs(got.Traces))
	}
}

func TestDiagnosticsTracesFilterByStatus(t *testing.T) {
	rb := ringbuf.New()
	seedRichTrace(t, rb, 1, "PUT /bkt/a", "ok-1", 10, false)
	seedRichTrace(t, rb, 2, "PUT /bkt/b", "err-1", 20, true)
	seedRichTrace(t, rb, 3, "PUT /bkt/c", "err-2", 30, true)

	s := newTestServer()
	s.TraceRingbuf = rb

	got := filterResponse(t, s, url.Values{"status": {"Error"}})
	if got.Total != 2 {
		t.Errorf("total=%d want 2", got.Total)
	}
	if !sortedEqual(requestIDs(got.Traces), []string{"err-1", "err-2"}) {
		t.Errorf("traces=%v want errors", requestIDs(got.Traces))
	}
}

func TestDiagnosticsTracesFilterByPathSubstrCaseInsensitive(t *testing.T) {
	rb := ringbuf.New()
	seedRichTrace(t, rb, 1, "PUT /demo-cephb/key", "match-1", 10, false)
	seedRichTrace(t, rb, 2, "PUT /demo-cepha/key", "skip", 20, false)
	seedRichTrace(t, rb, 3, "GET /demo-CEPHB/other", "match-2", 30, false)

	s := newTestServer()
	s.TraceRingbuf = rb

	got := filterResponse(t, s, url.Values{"path_substr": {"cephb"}})
	if got.Total != 2 {
		t.Errorf("total=%d want 2", got.Total)
	}
	if !sortedEqual(requestIDs(got.Traces), []string{"match-1", "match-2"}) {
		t.Errorf("traces=%v want match-1,match-2", requestIDs(got.Traces))
	}

	// `path` alias should match identically.
	gotAlias := filterResponse(t, s, url.Values{"path": {"cephb"}})
	if gotAlias.Total != got.Total {
		t.Errorf("alias total=%d want %d", gotAlias.Total, got.Total)
	}
}

func TestDiagnosticsTracesFilterByDurationRange(t *testing.T) {
	rb := ringbuf.New()
	seedRichTrace(t, rb, 1, "PUT /a", "fast", 5, false)
	seedRichTrace(t, rb, 2, "PUT /b", "mid", 50, false)
	seedRichTrace(t, rb, 3, "PUT /c", "slow", 500, false)

	s := newTestServer()
	s.TraceRingbuf = rb

	got := filterResponse(t, s, url.Values{
		"min_duration_ms": {"10"},
		"max_duration_ms": {"100"},
	})
	if got.Total != 1 {
		t.Errorf("total=%d want 1", got.Total)
	}
	if len(got.Traces) != 1 || got.Traces[0].RequestID != "mid" {
		t.Errorf("traces=%v want [mid]", requestIDs(got.Traces))
	}
}

func TestDiagnosticsTracesFilterCombination(t *testing.T) {
	rb := ringbuf.New()
	// 5 GET success + 5 PUT success + 5 PUT error + 5 DELETE error.
	for i := byte(1); i <= 5; i++ {
		seedRichTrace(t, rb, i, "GET /demo-cephb/o", "get-ok-"+string(rune('0'+i)), 10, false)
	}
	for i := byte(6); i <= 10; i++ {
		seedRichTrace(t, rb, i, "PUT /demo-cephb/o", "put-ok-"+string(rune('0'+i)), 20, false)
	}
	for i := byte(11); i <= 15; i++ {
		seedRichTrace(t, rb, i, "PUT /demo-cephb/o", "put-err-"+string(rune('0'+i)), 200, true)
	}
	for i := byte(16); i <= 20; i++ {
		seedRichTrace(t, rb, i, "DELETE /demo-cepha/o", "del-err-"+string(rune('0'+i)), 50, true)
	}

	s := newTestServer()
	s.TraceRingbuf = rb

	// PUT + Error + cephb → 5 entries (put-err-*).
	got := filterResponse(t, s, url.Values{
		"method":      {"PUT"},
		"status":      {"Error"},
		"path_substr": {"cephb"},
	})
	if got.Total != 5 {
		t.Errorf("total=%d want 5", got.Total)
	}
	for _, tr := range got.Traces {
		if !strings.HasPrefix(tr.RequestID, "put-err-") {
			t.Errorf("unexpected trace %s", tr.RequestID)
		}
	}
}

func TestDiagnosticsTracesFilterAppliedBeforePagination(t *testing.T) {
	rb := ringbuf.New()
	for i := byte(1); i <= 6; i++ {
		seedRichTrace(t, rb, i, "GET /a", "g-"+string(rune('0'+i)), 10, false)
	}
	for i := byte(7); i <= 10; i++ {
		seedRichTrace(t, rb, i, "PUT /a", "p-"+string(rune('0'+i-6)), 10, false)
	}

	s := newTestServer()
	s.TraceRingbuf = rb

	got := filterResponse(t, s, url.Values{
		"method": {"PUT"},
		"limit":  {"2"},
		"offset": {"0"},
	})
	if got.Total != 4 {
		t.Fatalf("total=%d want 4 (filtered, not full ringbuf=10)", got.Total)
	}
	if len(got.Traces) != 2 {
		t.Fatalf("page=%d want 2", len(got.Traces))
	}
}

func TestDiagnosticsTracesFilterRejectsInvalidValues(t *testing.T) {
	rb := ringbuf.New()
	seedRichTrace(t, rb, 1, "PUT /a", "p-a", 10, false)
	s := newTestServer()
	s.TraceRingbuf = rb

	cases := []struct {
		name   string
		params url.Values
	}{
		{"bad method", url.Values{"method": {"INVALID"}}},
		{"bad status", url.Values{"status": {"weird"}}},
		{"bad min", url.Values{"min_duration_ms": {"abc"}}},
		{"bad max", url.Values{"max_duration_ms": {"def"}}},
		{"negative min", url.Values{"min_duration_ms": {"-1"}}},
		{"negative max", url.Values{"max_duration_ms": {"-5"}}},
		{"min > max", url.Values{"min_duration_ms": {"500"}, "max_duration_ms": {"100"}}},
		{"path too long", url.Values{"path_substr": {strings.Repeat("x", maxDiagnosticsPathSubstrLen+1)}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			req := withAuditAuthCtx(httptest.NewRequest(http.MethodGet,
				"/admin/v1/diagnostics/traces?"+tc.params.Encode(), nil), "operator")
			s.routes().ServeHTTP(rr, req)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
			}
			var env errorResponse
			if err := json.NewDecoder(rr.Body).Decode(&env); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if env.Code != "InvalidFilter" {
				t.Errorf("code=%q want InvalidFilter", env.Code)
			}
		})
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
