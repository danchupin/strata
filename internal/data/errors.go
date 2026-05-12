package data

import "errors"

// ErrNotFound is returned by data.Backend implementations when the
// requested object does not exist (e.g. backend NoSuchKey). Gateway
// callers map this to S3 404 NoSuchKey instead of 500.
var ErrNotFound = errors.New("data: object not found")

// ErrClusterStatsNotSupported is returned by data backends that have no
// way to surface cluster-fill telemetry (today: memory, s3). The
// rebalance worker (US-006) treats this as "OK to proceed" — the
// target-full safety rail only fires for backends with real probes.
var ErrClusterStatsNotSupported = errors.New("data: cluster stats not supported")
