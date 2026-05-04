package adminapi

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/danchupin/strata/internal/auditstream"
)

// auditStreamKeepAliveInterval is the cadence for ":keep-alive\n\n" pings.
// Defeats idle-connection proxies that drop quiet long-lived HTTP responses.
const auditStreamKeepAliveInterval = 25 * time.Second

// handleAuditStream serves GET /admin/v1/audit/stream as Server-Sent Events.
// Each persisted audit row becomes one `data: <json>\n\n` frame on every
// matching subscriber. Filters (action / principal / bucket) are applied
// server-side by the broadcaster — filtered subscribers do not receive
// irrelevant frames. Closes on context cancel; idle connections receive a
// ":keep-alive\n\n" ping every 25s so reverse proxies do not drop them.
//
// 503 ServiceUnavailable when the broadcaster is unconfigured (e.g. unit
// tests bypassing serverapp wiring). 500 Internal when the underlying
// ResponseWriter does not support flushing — required for SSE to deliver
// frames incrementally.
func (s *Server) handleAuditStream(w http.ResponseWriter, r *http.Request) {
	if s.AuditStream == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "Unavailable", "audit stream not configured")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSONError(w, http.StatusInternalServerError, "Internal", "streaming unsupported")
		return
	}

	q := r.URL.Query()
	filter := auditstream.Filter{
		Action:    q.Get("action"),
		Principal: q.Get("principal"),
		Bucket:    q.Get("bucket"),
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ch := s.AuditStream.Subscribe(r.Context(), filter)
	keepAlive := time.NewTicker(s.auditStreamKeepAlive())
	defer keepAlive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-ch:
			if ev == nil {
				continue
			}
			buf, err := json.Marshal(auditRecordToJSON(*ev))
			if err != nil {
				continue
			}
			if _, err := w.Write([]byte("data: ")); err != nil {
				return
			}
			if _, err := w.Write(buf); err != nil {
				return
			}
			if _, err := w.Write([]byte("\n\n")); err != nil {
				return
			}
			flusher.Flush()
		case <-keepAlive.C:
			if _, err := w.Write([]byte(":keep-alive\n\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// auditStreamKeepAlive returns the keep-alive ticker interval; tests override
// via Server.AuditStreamKeepAliveInterval to drive a sub-second ping.
func (s *Server) auditStreamKeepAlive() time.Duration {
	if s.AuditStreamKeepAliveInterval > 0 {
		return s.AuditStreamKeepAliveInterval
	}
	return auditStreamKeepAliveInterval
}
