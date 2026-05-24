package rados

import "sync/atomic"

// NextRoundRobin is the exported entry point for cephimpl/. Same behavior
// as the in-package helper.
func NextRoundRobin(counter *atomic.Uint64, size int) int { return nextRoundRobin(counter, size) }

// nextRoundRobin advances counter and returns its slot in [0, size).
// size must be > 0; callers guarantee non-empty pools at construction
// time. Tag-free so the round-robin contract can be exercised without
// librados.
func nextRoundRobin(counter *atomic.Uint64, size int) int {
	if size <= 0 {
		return 0
	}
	return int((counter.Add(1) - 1) % uint64(size))
}
