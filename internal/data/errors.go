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

// ErrDrainRefused is the sentinel returned by data backends when
// STRATA_DRAIN_STRICT=on and the placement picker fell back to a
// draining cluster. Carries the resolved cluster id in a
// DrainRefusedError wrapper accessible via errors.As. The gateway
// maps this to HTTP 503 ServiceUnavailable + Retry-After: 300 on the
// PUT chunks hot path. ONLY raised on writes — reads / deletes /
// multipart finalisation continue to work against draining clusters.
var ErrDrainRefused = errors.New("data: PUT refused — target cluster is draining (STRATA_DRAIN_STRICT=on)")

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
	return fmt.Sprintf("data: PUT refused — cluster %q is draining (STRATA_DRAIN_STRICT=on)", e.Cluster)
}

// Unwrap lets errors.Is(err, ErrDrainRefused) detect this error class
// without callers reaching for errors.As when they don't need the
// cluster id.
func (e *DrainRefusedError) Unwrap() error { return ErrDrainRefused }

// ParseDrainStrict parses the STRATA_DRAIN_STRICT env-style flag.
// Accepts "on"/"off"/"true"/"false" (case-insensitive); empty string
// returns false (default off). Unknown values return an error so the
// gateway fails-fast at boot rather than silently choosing a default.
func ParseDrainStrict(raw string) (bool, error) {
	switch raw {
	case "", "off", "OFF", "Off", "false", "False", "FALSE":
		return false, nil
	case "on", "ON", "On", "true", "True", "TRUE":
		return true, nil
	default:
		return false, fmt.Errorf("STRATA_DRAIN_STRICT: unknown value %q (expected on/off/true/false)", raw)
	}
}
