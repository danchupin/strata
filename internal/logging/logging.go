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
	"time"

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

// NewWithLevel builds a slog.Logger writing to w at the supplied level. Format
// selects the handler shape: "text" produces slog.NewTextHandler output, any
// other value (default "json") produces slog.NewJSONHandler. Pass the level
// already resolved by config.Load — this entry point trusts the caller.
func NewWithLevel(w io.Writer, level slog.Level, format string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level}
	if strings.EqualFold(strings.TrimSpace(format), "text") {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts))
}

// SetupWithLevel builds the logger via NewWithLevel(os.Stdout, level, format)
// and installs it as slog.Default. Prefer this over Setup when a resolved
// *config.Config is already in hand — skips the env re-read so TOML-only
// boot honours the configured log level.
func SetupWithLevel(level slog.Level, format string) *slog.Logger {
	l := NewWithLevel(os.Stdout, level, format)
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

// statusWriter shims http.ResponseWriter to capture the status code so the
// per-request access-log line below records it. Defaults to 200 because Go's
// http.ResponseWriter contract treats Write without WriteHeader as 200 OK.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (sw *statusWriter) WriteHeader(code int) {
	sw.status = code
	sw.ResponseWriter.WriteHeader(code)
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
	reqLogger := logger.With("request_id", id)
	ctx = WithLogger(ctx, reqLogger)
	sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
	start := time.Now()
	m.Next.ServeHTTP(sw, r.WithContext(ctx))
	reqLogger.InfoContext(ctx, "request",
		"method", r.Method,
		"path", r.URL.Path,
		"status", sw.status,
		"duration_ms", time.Since(start).Milliseconds(),
	)
}
