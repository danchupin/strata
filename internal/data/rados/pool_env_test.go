package rados

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestPoolSizeFromEnvDefault(t *testing.T) {
	t.Setenv("STRATA_RADOS_POOL_SIZE", "")
	if got := poolSizeFromEnv(nil); got != DefaultPoolSize {
		t.Fatalf("unset env: want %d got %d", DefaultPoolSize, got)
	}
}

func TestPoolSizeFromEnvValid(t *testing.T) {
	t.Setenv("STRATA_RADOS_POOL_SIZE", "4")
	if got := poolSizeFromEnv(nil); got != 4 {
		t.Fatalf("env=4: want 4 got %d", got)
	}
}

func TestPoolSizeFromEnvClampLow(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	t.Setenv("STRATA_RADOS_POOL_SIZE", "0")
	if got := poolSizeFromEnv(logger); got != 1 {
		t.Fatalf("env=0: want clamp to 1 got %d", got)
	}
	if !strings.Contains(buf.String(), "below range") {
		t.Fatalf("expected WARN about clamp, got %q", buf.String())
	}
}

func TestPoolSizeFromEnvClampHigh(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	t.Setenv("STRATA_RADOS_POOL_SIZE", "999")
	if got := poolSizeFromEnv(logger); got != MaxPoolSize {
		t.Fatalf("env=999: want clamp to %d got %d", MaxPoolSize, got)
	}
	if !strings.Contains(buf.String(), "above range") {
		t.Fatalf("expected WARN about clamp, got %q", buf.String())
	}
}

func TestPoolSizeFromEnvUnparseable(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	t.Setenv("STRATA_RADOS_POOL_SIZE", "abc")
	if got := poolSizeFromEnv(logger); got != DefaultPoolSize {
		t.Fatalf("env=abc: want default %d got %d", DefaultPoolSize, got)
	}
	if !strings.Contains(buf.String(), "unparseable") {
		t.Fatalf("expected WARN about parse, got %q", buf.String())
	}
}

func TestClampPoolSizeNilLogger(t *testing.T) {
	if got := clampPoolSize(0, nil); got != 1 {
		t.Fatalf("nil logger clamp low: want 1 got %d", got)
	}
	if got := clampPoolSize(99, nil); got != MaxPoolSize {
		t.Fatalf("nil logger clamp high: want %d got %d", MaxPoolSize, got)
	}
	if got := clampPoolSize(5, nil); got != 5 {
		t.Fatalf("in-range: want 5 got %d", got)
	}
}
