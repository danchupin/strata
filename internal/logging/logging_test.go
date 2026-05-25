package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseLevel(t *testing.T) {
	cases := []struct {
		in   string
		want slog.Level
	}{
		{"", slog.LevelInfo},
		{"DEBUG", slog.LevelDebug},
		{"debug", slog.LevelDebug},
		{"INFO", slog.LevelInfo},
		{"WARN", slog.LevelWarn},
		{"warning", slog.LevelWarn},
		{"ERROR", slog.LevelError},
		{"  Error ", slog.LevelError},
		{"random", slog.LevelInfo},
	}
	for _, c := range cases {
		if got := ParseLevel(c.in); got != c.want {
			t.Errorf("ParseLevel(%q)=%v want %v", c.in, got, c.want)
		}
	}
}

func TestMiddlewareGeneratesUUIDWhenHeaderMissing(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	var seenID string
	var seenLogID string
	mw := NewMiddleware(logger, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenID = RequestIDFromContext(r.Context())
		LoggerFromContext(r.Context()).Info("handled")
		seenLogID = r.Header.Get(HeaderRequestID)
	}))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	mw.ServeHTTP(rr, req)

	if seenID == "" {
		t.Fatal("request id not attached to context")
	}
	if seenLogID != seenID {
		t.Fatalf("inbound header %q != ctx id %q", seenLogID, seenID)
	}
	if got := rr.Header().Get(HeaderRequestID); got != seenID {
		t.Fatalf("response header %q != ctx id %q", got, seenID)
	}

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) < 2 {
		t.Fatalf("want >=2 log lines (handler + access-log), got %d: %q", len(lines), buf.String())
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatalf("handler log line not JSON: %v: %q", err, lines[0])
	}
	if rec["request_id"] != seenID {
		t.Fatalf("handler log request_id=%v want %s", rec["request_id"], seenID)
	}
	// Access-log line emitted by the middleware itself; verify shape.
	var access map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &access); err != nil {
		t.Fatalf("access log line not JSON: %v: %q", err, lines[len(lines)-1])
	}
	if access["msg"] != "request" {
		t.Fatalf("access log msg=%v want \"request\"", access["msg"])
	}
	if access["path"] != "/" || access["method"] != "GET" || access["status"] != float64(200) {
		t.Fatalf("access log fields wrong: %v", access)
	}
	if _, ok := access["duration_ms"]; !ok {
		t.Fatalf("access log missing duration_ms: %v", access)
	}
}

func TestMiddlewareReusesInboundHeader(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	const want = "client-supplied-id"

	var ctxID string
	mw := NewMiddleware(logger, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		ctxID = RequestIDFromContext(r.Context())
	}))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(HeaderRequestID, want)
	mw.ServeHTTP(rr, req)

	if ctxID != want {
		t.Fatalf("ctx id=%q want %q", ctxID, want)
	}
	if got := rr.Header().Get(HeaderRequestID); got != want {
		t.Fatalf("response header=%q want %q", got, want)
	}
}

func TestMiddlewareSameIDAcrossLogLines(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	mw := NewMiddleware(logger, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		l := LoggerFromContext(r.Context())
		l.Info("first")
		l.Info("second")
		l.Info("third")
	}))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(HeaderRequestID, "abc-123")
	mw.ServeHTTP(rr, req)

	var ids []string
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("bad json: %v: %q", err, line)
		}
		ids = append(ids, rec["request_id"].(string))
	}
	// 3 lines from handler + 1 access-log line emitted by the middleware itself.
	if len(ids) != 4 {
		t.Fatalf("got %d log lines want 4 (3 handler + 1 middleware access-log)", len(ids))
	}
	for _, id := range ids {
		if id != "abc-123" {
			t.Fatalf("log id=%q want abc-123", id)
		}
	}
}

func TestLoggerFromContextDefault(t *testing.T) {
	l := LoggerFromContext(httptest.NewRequest(http.MethodGet, "/", nil).Context())
	if l == nil {
		t.Fatal("LoggerFromContext returned nil")
	}
}
