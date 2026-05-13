package data

import (
	"errors"
	"testing"
)

func TestDrainRefusedErrorIs(t *testing.T) {
	err := NewDrainRefusedError("default")
	if !errors.Is(err, ErrDrainRefused) {
		t.Fatalf("errors.Is(NewDrainRefusedError, ErrDrainRefused): false")
	}
	var dre *DrainRefusedError
	if !errors.As(err, &dre) {
		t.Fatalf("errors.As(NewDrainRefusedError, *DrainRefusedError): false")
	}
	if dre.Cluster != "default" {
		t.Fatalf("DrainRefusedError.Cluster: want %q, got %q", "default", dre.Cluster)
	}
}
