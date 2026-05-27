package serverapp

import (
	"bytes"
	"os"
	"runtime/pprof"
	"testing"

	gpprof "github.com/google/pprof/profile"

	"github.com/danchupin/strata/internal/pprofutil"
)

// parsePprofBytes is the in-package shim onto internal/pprofutil.Parse —
// keeps the test call sites short while the public decoder stays
// reusable from smoke scripts + future operator tooling.
func parsePprofBytes(b []byte) (*gpprof.Profile, error) {
	return pprofutil.Parse(b)
}

// TestPprofDecode is the Go-native pprof decode smoke. Reads a pprof file
// from STRATA_PPROF_SMOKE_PROFILE (set by scripts/smoke-pprof.sh) and
// asserts the profile parses cleanly. Skipped when the env is unset so
// `go test ./...` from the dev loop keeps a flat exit.
//
// Operators on environments without `go tool pprof` reuse the decoder via
// `go test -run TestPprofDecode -count=1 ./internal/serverapp/...` with
// STRATA_PPROF_SMOKE_PROFILE pointing at the captured profile.
func TestPprofDecode(t *testing.T) {
	path := os.Getenv("STRATA_PPROF_SMOKE_PROFILE")
	if path == "" {
		t.Skip("STRATA_PPROF_SMOKE_PROFILE unset; skipping (smoke entry only)")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	prof, err := parsePprofBytes(data)
	if err != nil {
		t.Fatalf("parse pprof %s: %v", path, err)
	}
	if len(prof.SampleType) == 0 {
		t.Errorf("pprof %s has no sample types — likely malformed", path)
	}
}

// TestPprofDecodeRoundTrip is a sanity check that the parser handles an
// in-process WriteHeapProfile dump. Catches regressions in the google/pprof
// dep before the smoke surface fires.
func TestPprofDecodeRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := pprof.Lookup("heap").WriteTo(&buf, 0); err != nil {
		t.Fatalf("write heap profile: %v", err)
	}
	prof, err := parsePprofBytes(buf.Bytes())
	if err != nil {
		t.Fatalf("parse heap profile: %v", err)
	}
	if len(prof.SampleType) == 0 {
		t.Fatal("heap profile has no SampleType — unexpected for an in-process dump")
	}
}
