package tikv

import (
	"context"
	"errors"
	"testing"
)

// TestProbeStubReturnsErrUnsupported asserts the skeleton is wired —
// US-001's stop condition. Real Probe lands with US-003 once Open
// connects to PD.
func TestProbeStubReturnsErrUnsupported(t *testing.T) {
	s, err := Open(Config{PDEndpoints: []string{"pd:2379"}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	if got := s.Probe(context.Background()); !errors.Is(got, errors.ErrUnsupported) {
		t.Fatalf("Probe returned %v, want errors.ErrUnsupported", got)
	}
}
