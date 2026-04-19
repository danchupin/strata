package data

import (
	"context"
	"io"
)

type Backend interface {
	PutChunks(ctx context.Context, r io.Reader, class string) (*Manifest, error)
	GetChunks(ctx context.Context, m *Manifest, offset, length int64) (io.ReadCloser, error)
	Delete(ctx context.Context, m *Manifest) error
	Close() error
}
