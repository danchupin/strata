package s3

import (
	"bytes"
	"context"
	"errors"
	"testing"

	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/danchupin/strata/internal/data"
)

// TestStubReturnsErrUnsupported pins the US-001 acceptance: a New() stub
// must satisfy data.Backend and surface errors.ErrUnsupported on every
// mutating method until the real implementation lands.
func TestStubReturnsErrUnsupported(t *testing.T) {
	b := New()

	var _ data.Backend = b

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

// TestStubPutReturnsErrUnsupported guards the US-002 streaming Put: a
// stub Backend (no Open) must not silently succeed — callers without a
// live S3 client get errors.ErrUnsupported.
func TestStubPutReturnsErrUnsupported(t *testing.T) {
	b := New()

	_, err := b.Put(context.Background(), "k", bytes.NewReader(nil), 0)
	if !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("Put: want errors.ErrUnsupported, got %v", err)
	}
}

// TestStubGetReturnsErrUnsupported guards US-003: a stub Backend (no
// Open) must surface errors.ErrUnsupported on Get / GetRange — never
// silently succeed.
func TestStubGetReturnsErrUnsupported(t *testing.T) {
	b := New()
	ctx := context.Background()

	if _, err := b.Get(ctx, "k"); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("Get: want errors.ErrUnsupported, got %v", err)
	}
	if _, err := b.GetRange(ctx, "k", 0, 1); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("GetRange: want errors.ErrUnsupported, got %v", err)
	}
}

// TestGetRangeValidatesArguments pins US-003 input validation: negative
// offset or non-positive length is a programmer error and must fail
// before any network call.
func TestGetRangeValidatesArguments(t *testing.T) {
	b := &Backend{bucket: "b", client: &awss3.Client{}}
	ctx := context.Background()

	if _, err := b.GetRange(ctx, "k", -1, 1); err == nil {
		t.Fatal("GetRange with negative offset: want error, got nil")
	}
	if _, err := b.GetRange(ctx, "k", 0, 0); err == nil {
		t.Fatal("GetRange with zero length: want error, got nil")
	}
	if _, err := b.GetRange(ctx, "k", 0, -5); err == nil {
		t.Fatal("GetRange with negative length: want error, got nil")
	}
}

// TestOpenValidatesRequiredConfig pins the US-002 fail-fast contract:
// missing bucket / region must error at construction, not at first Put.
func TestOpenValidatesRequiredConfig(t *testing.T) {
	ctx := context.Background()

	if _, err := Open(ctx, Config{Region: "us-east-1"}); err == nil {
		t.Fatal("Open with empty bucket: want error, got nil")
	}
	if _, err := Open(ctx, Config{Bucket: "x"}); err == nil {
		t.Fatal("Open with empty region: want error, got nil")
	}
}
