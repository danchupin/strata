package data

import (
	"errors"
	"fmt"
)

// ErrNotFound is returned by data.Backend implementations when the
// requested object does not exist (e.g. backend NoSuchKey). Gateway
// callers map this to S3 404 NoSuchKey instead of 500.
var ErrNotFound = errors.New("data: object not found")

// ErrChunkNotFound is the backend-agnostic sentinel returned by
// data.Backend.Delete when the requested chunk is already absent on the
// underlying store (RADOS goceph.ErrNotFound, S3 NoSuchKey, etc). The gc
// worker treats this as terminal: ack the queue entry so a chunk swept
// by a sibling leader doesn't loop forever.
var ErrChunkNotFound = errors.New("data: chunk not found")

// ErrClusterStatsNotSupported is returned by data backends that have no
// way to surface cluster-fill telemetry (today: memory, s3). The
// rebalance worker (US-006) treats this as "OK to proceed" — the
// target-full safety rail only fires for backends with real probes.
var ErrClusterStatsNotSupported = errors.New("data: cluster stats not supported")

// ErrClusterUnknown signals a ClusterECCapability call against a
// cluster id the backend does not have configured (US-007 EC-aware
// manifests).
var ErrClusterUnknown = errors.New("data: unknown cluster id")

// ErrRADOSNotCompiled is the cross-package sentinel returned by the
// main rados package's stub New() when the binary was built without the
// librados-linked cephimpl/ module. The real backend lives in
// `github.com/danchupin/strata/cephimpl`; callers that need it (the
// gateway, bench tooling) import cephimpl directly under a ceph build
// tag and route around this stub.
var ErrRADOSNotCompiled = errors.New("data: rados backend not compiled (use cephimpl/ + go.work)")

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

// ErrChecksumMismatch is the sentinel returned by the read path
// (GetChunks) when a stored chunk's bytes no longer match the CRC32C
// recorded on its ChunkRef at PutChunks time (US-009). It signals
// at-rest corruption (a plaintext byte-flip) and MUST fail the read
// loud — surfaced as a 5xx / aborted stream, never a silent short or a
// corrupted 200. SSE objects are unaffected (their AEAD tag already
// detects tampering before plaintext is emitted).
var ErrChecksumMismatch = errors.New("data: chunk checksum mismatch (at-rest corruption)")

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
