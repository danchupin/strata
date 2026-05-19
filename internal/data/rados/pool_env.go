package rados

import (
	"log/slog"
	"os"
	"strconv"
)

const (
	// DefaultPoolSize keeps the historical single-conn shape: one
	// librados *Conn per cluster. Operators flip
	// STRATA_RADOS_POOL_SIZE to opt into the round-robin pool.
	DefaultPoolSize = 1
	// MaxPoolSize bounds the pool top-end to keep per-cluster cephx
	// session state + per-conn thread pool from running away. 32 is
	// chosen to comfortably cover the largest expected concurrent
	// PutChunks fan-out (defaultPutConcurrency=32).
	MaxPoolSize = 32
)

// poolSizeFromEnv reads STRATA_RADOS_POOL_SIZE and clamps to
// [1, MaxPoolSize]. Unset / unparseable falls back to DefaultPoolSize.
// Out-of-range values are clamped + WARN-logged via the supplied logger
// (nil-safe).
func poolSizeFromEnv(logger *slog.Logger) int {
	raw := os.Getenv("STRATA_RADOS_POOL_SIZE")
	if raw == "" {
		return DefaultPoolSize
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		if logger != nil {
			logger.Warn("STRATA_RADOS_POOL_SIZE unparseable; using default",
				"value", raw, "default", DefaultPoolSize)
		}
		return DefaultPoolSize
	}
	return clampPoolSize(n, logger)
}

func clampPoolSize(n int, logger *slog.Logger) int {
	if n < 1 {
		if logger != nil {
			logger.Warn("STRATA_RADOS_POOL_SIZE below range; clamped",
				"value", n, "min", 1)
		}
		return 1
	}
	if n > MaxPoolSize {
		if logger != nil {
			logger.Warn("STRATA_RADOS_POOL_SIZE above range; clamped",
				"value", n, "max", MaxPoolSize)
		}
		return MaxPoolSize
	}
	return n
}
