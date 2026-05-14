package data

import (
	"errors"
	"fmt"
)

// ErrNotFound is returned by data.Backend implementations when the
// requested object does not exist (e.g. backend NoSuchKey). Gateway
// callers map this to S3 404 NoSuchKey instead of 500.
var ErrNotFound = errors.New("data: object not found")

// ErrClusterStatsNotSupported is returned by data backends that have no
// way to surface cluster-fill telemetry (today: memory, s3). The
// rebalance worker (US-006) treats this as "OK to proceed" — the
// target-full safety rail only fires for backends with real probes.
var ErrClusterStatsNotSupported = errors.New("data: cluster stats not supported")

// ErrDrainRefused is the sentinel returned by data backends when the
// placement picker fell back to a draining cluster on the PUT hot path.
// Carries the resolved cluster id in a DrainRefusedError wrapper
// accessible via errors.As. The gateway maps this to HTTP 503
// ServiceUnavailable + Retry-After: 300 on the PUT chunks hot path.
// ONLY raised on writes — reads / deletes / multipart finalisation
// continue to work against draining clusters (in-flight multipart
// sessions land on the original cluster via the persisted handle and
// never re-consult the picker).
var ErrDrainRefused = errors.New("data: PUT refused — target cluster is draining")

// DrainRefusedError carries the resolved cluster id alongside
// ErrDrainRefused so the gateway can surface it on the wire and the
// metric stamp the cluster label. Implementations construct via
// NewDrainRefusedError.
type DrainRefusedError struct {
	Cluster string
}

// NewDrainRefusedError builds a DrainRefusedError pointing at the
// supplied cluster id.
func NewDrainRefusedError(cluster string) *DrainRefusedError {
	return &DrainRefusedError{Cluster: cluster}
}

func (e *DrainRefusedError) Error() string {
	return fmt.Sprintf("data: PUT refused — cluster %q is draining", e.Cluster)
}

// Unwrap lets errors.Is(err, ErrDrainRefused) detect this error class
// without callers reaching for errors.As when they don't need the
// cluster id.
func (e *DrainRefusedError) Unwrap() error { return ErrDrainRefused }
