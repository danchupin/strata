package s3

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/danchupin/strata/internal/data"
)

// TestStubReturnsErrUnsupported pins the US-001 acceptance: the stub must
// be wired into the data.Backend contract and every mutating method must
// surface errors.ErrUnsupported until later stories fill them in.
func TestStubReturnsErrUnsupported(t *testing.T) {
	b := New()

	var _ data.Backend = b // belt-and-suspenders alongside the package-level assertion

	ctx := context.Background()

	if _, err := b.PutChunks(ctx, bytes.NewReader(nil), "STANDARD"); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("PutChunks: want errors.ErrUnsupported, got %v", err)
	}
	if _, err := b.GetChunks(ctx, &data.Manifest{}, 0, 0); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("GetChunks: want errors.ErrUnsupported, got %v", err)
	}
	if err := b.Delete(ctx, &data.Manifest{}); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("Delete: want errors.ErrUnsupported, got %v", err)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("Close: want nil, got %v", err)
	}
}
