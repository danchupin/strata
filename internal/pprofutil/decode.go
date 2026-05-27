// Package pprofutil exposes a Go-native pprof decode helper backed by
// github.com/google/pprof/profile. Used by serverapp tests + the
// scripts/smoke-pprof.sh smoke (entrypoint TestPprofDecode) so neither
// path depends on a runtime `go tool pprof` binary or BusyBox-fragile
// shell tooling.
//
// Promoting google/pprof to a direct require lives here because the
// dependency would otherwise be marked `// indirect` — its only callers
// would be test files, which `go mod tidy` flags as indirect even though
// they belong to the main module. Putting Parse in a non-test source
// file resolves the marker.
package pprofutil

import (
	"bytes"
	"errors"

	gpprof "github.com/google/pprof/profile"
)

// Parse decodes a pprof binary payload returned by /debug/pprof/* into a
// *profile.Profile. Empty input is an error (rejects the operator-visible
// case of pointing at the wrong listener / a misconfigured endpoint).
func Parse(b []byte) (*gpprof.Profile, error) {
	if len(b) == 0 {
		return nil, errors.New("empty pprof body")
	}
	return gpprof.Parse(bytes.NewReader(b))
}
