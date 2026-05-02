// Package logging centralises slog setup, log-level parsing, and the
// request-id HTTP middleware that threads a per-request UUID through every
// downstream code path.
package logging

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/google/uuid"
)

// HeaderRequestID is the HTTP header carrying the per-request id. Clients may
// supply their own; the middleware generates a UUID when absent.
const HeaderRequestID = "X-Request-Id"

// EnvLogLevel selects the slog level. Defaults to INFO when unset or invalid.
const EnvLogLevel = "STRATA_LOG_LEVEL"

// ParseLevel maps a STRATA_LOG_LEVEL value to slog.Level.
// Empty / unknown -> INFO.
func ParseLevel(s string) slog.Level {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "DEBUG":
		return slog.LevelDebug
	case "WARN", "WARNING":
		return slog.LevelWarn
	case "ERROR":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// New builds a JSON slog.Logger writing to w with the level taken from
// STRATA_LOG_LEVEL. Pass os.Stdout in production.
func New(w io.Writer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: ParseLevel(os.Getenv(EnvLogLevel))}))
}

// Setup builds the logger via New(os.Stdout) and installs it as slog.Default.
// Returns the logger so callers can also pass it explicitly.
func Setup() *slog.Logger {
	l := New(os.Stdout)
	slog.SetDefault(l)
	return l
}

type ctxKey int

const (
	ctxKeyRequestID ctxKey = iota
	ctxKeyLogger
)

// WithRequestID attaches id to ctx.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKeyRequestID, id)
}

// RequestIDFromContext returns the id attached by Middleware, or "".
func RequestIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyRequestID).(string); ok {
		return v
	}
	return ""
}

// WithLogger attaches a logger to ctx. Use slog.With on the parent logger
// before calling so request-scoped attributes (request_id, principal) ride
// through.
func WithLogger(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKeyLogger, l)
}

// LoggerFromContext returns the logger attached by Middleware, falling back to
// slog.Default when nothing is attached.
func LoggerFromContext(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(ctxKeyLogger).(*slog.Logger); ok && l != nil {
		return l
	}
	return slog.Default()
}

// Middleware reads X-Request-Id (or generates a UUID), attaches it to the
// request context + headers, and stores a child slog.Logger with a
// "request_id" attribute on the context so downstream handlers see correlated
// logs without further plumbing. Both the inbound r.Header and the response
// writer headers carry the resolved id so existing code paths (e.g. the
// access-log middleware) keep working.
type Middleware struct {
	Logger *slog.Logger
	Next   http.Handler
	NewID  func() string
}

// NewMiddleware wraps next. logger is the per-binary base logger; NewID
// defaults to uuid.NewString.
func NewMiddleware(logger *slog.Logger, next http.Handler) *Middleware {
	if logger == nil {
		logger = slog.Default()
	}
	return &Middleware{Logger: logger, Next: next, NewID: uuid.NewString}
}

func (m *Middleware) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id := r.Header.Get(HeaderRequestID)
	if id == "" {
		newID := m.NewID
		if newID == nil {
			newID = uuid.NewString
		}
		id = newID()
		r.Header.Set(HeaderRequestID, id)
	}
	w.Header().Set(HeaderRequestID, id)
	ctx := WithRequestID(r.Context(), id)
	logger := m.Logger
	if logger == nil {
		logger = slog.Default()
	}
	ctx = WithLogger(ctx, logger.With("request_id", id))
	m.Next.ServeHTTP(w, r.WithContext(ctx))
}
