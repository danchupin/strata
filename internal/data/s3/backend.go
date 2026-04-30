// Package s3 implements an S3-compatible data backend for Strata.
//
// US-001 lays down the package skeleton: the Backend type satisfies
// data.Backend, every method returns errors.ErrUnsupported, and no SDK
// client is constructed yet. Subsequent stories (US-002..US-010) flesh out
// the real implementation against aws-sdk-go-v2 and wire dispatch in
// internal/serverapp (US-009).
package s3

import (
	"context"
	"errors"
	"io"

	"github.com/danchupin/strata/internal/data"
)

// Backend is the S3-over-S3 data backend. Stub-only in US-001.
type Backend struct{}

// New constructs a Backend. The argument is reserved — US-005 wires real
// configuration via koanf. Until then, the constructor returns a usable
// stub whose methods all error with errors.ErrUnsupported.
func New() *Backend {
	return &Backend{}
}

// Compile-time assertion that *Backend satisfies data.Backend.
var _ data.Backend = (*Backend)(nil)

func (b *Backend) PutChunks(ctx context.Context, r io.Reader, class string) (*data.Manifest, error) {
	return nil, errors.ErrUnsupported
}

func (b *Backend) GetChunks(ctx context.Context, m *data.Manifest, offset, length int64) (io.ReadCloser, error) {
	return nil, errors.ErrUnsupported
}

func (b *Backend) Delete(ctx context.Context, m *data.Manifest) error {
	return errors.ErrUnsupported
}

func (b *Backend) Close() error { return nil }
