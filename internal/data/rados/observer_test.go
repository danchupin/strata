package rados

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/danchupin/strata/internal/logging"
)

func TestLogOpDebugEmitsExpectedFields(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx := logging.WithRequestID(context.Background(), "req-rados")

	LogOp(ctx, logger, "put", "abc.00000", 12*time.Millisecond, nil)

	out := buf.String()
	for _, want := range []string{
		`"level":"DEBUG"`,
		`"msg":"rados: op"`,
		`"request_id":"req-rados"`,
		`"op":"put"`,
		`"oid":"abc.00000"`,
		`"duration_ms":12`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("log missing %q\nfull: %s", want, out)
		}
	}
}

func TestLogOpIncludesErrorWhenSet(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	LogOp(context.Background(), logger, "get", "x", 0, errors.New("boom"))

	if !strings.Contains(buf.String(), `"error":"boom"`) {
		t.Fatalf("missing error attr; got %s", buf.String())
	}
}

func TestLogOpSilentAtInfoLevel(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	LogOp(context.Background(), logger, "del", "x", 0, nil)

	if buf.Len() != 0 {
		t.Fatalf("expected no log at INFO; got %s", buf.String())
	}
}

func TestLogOpNilLoggerIsNoop(t *testing.T) {
	// Must not panic.
	LogOp(context.Background(), nil, "put", "x", time.Millisecond, nil)
}
