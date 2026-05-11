package otel

import (
	"net/http"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/danchupin/strata/internal/otel/ringbuf"
)

// NewMiddleware wraps next so each HTTP request runs inside a server
// span. Ingress traceparent headers (W3C) are honoured via the global
// propagator; absent headers yield a fresh root span. Span name is
// "<METHOD> <path>". The span captures http.method, http.target,
// http.host, and http.status_code; status >= 500 marks the span as
// Error so the tail-sampler always exports it.
func NewMiddleware(p *Provider, next http.Handler) http.Handler {
	return &middleware{
		tracer:     p.Tracer(TracerName),
		propagator: otel.GetTextMapPropagator(),
		next:       next,
	}
}

type middleware struct {
	tracer     trace.Tracer
	propagator propagation.TextMapPropagator
	next       http.Handler
}

func (m *middleware) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := m.propagator.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
	ctx, span := m.tracer.Start(ctx, r.Method+" "+r.URL.Path,
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(
			AttrComponentGateway,
			attribute.String("http.method", r.Method),
			attribute.String("http.target", r.URL.Path),
			attribute.String("http.host", r.Host),
		),
	)
	defer span.End()

	rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
	m.next.ServeHTTP(rec, r.WithContext(ctx))

	if id := r.Header.Get(headerRequestID); id != "" {
		span.SetAttributes(attribute.String(ringbuf.AttributeKeyRequestID, id))
	}
	span.SetAttributes(attribute.Int("http.status_code", rec.code))
	if rec.code >= 500 {
		span.SetStatus(codes.Error, http.StatusText(rec.code))
	}
}

// headerRequestID is the standard X-Request-Id header. Hard-coded here to
// avoid importing internal/logging from the otel package; the value matches
// logging.HeaderRequestID and tests pin both ends.
const headerRequestID = "X-Request-Id"

type statusRecorder struct {
	http.ResponseWriter
	code    int
	written bool
}

func (s *statusRecorder) WriteHeader(c int) {
	if !s.written {
		s.code = c
		s.written = true
	}
	s.ResponseWriter.WriteHeader(c)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.written {
		s.written = true
	}
	return s.ResponseWriter.Write(b)
}
