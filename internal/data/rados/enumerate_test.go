package rados

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/data"
)

func TestParseChunkOID(t *testing.T) {
	validUUID := "3f2504e0-4f89-41d3-9a0c-0305e82c3301"
	tests := []struct {
		name          string
		oid           string
		expectedOK    bool
		expectedIndex int
	}{
		{
			name:          "canonical chunk OID",
			oid:           validUUID + ".00000",
			expectedOK:    true,
			expectedIndex: 0,
		},
		{
			name:          "non-zero index",
			oid:           validUUID + ".00042",
			expectedOK:    true,
			expectedIndex: 42,
		},
		{
			name:          "index wider than pad",
			oid:           validUUID + ".123456",
			expectedOK:    true,
			expectedIndex: 123456,
		},
		{
			name:       "no dot separator",
			oid:        validUUID,
			expectedOK: false,
		},
		{
			name:       "index too short",
			oid:        validUUID + ".001",
			expectedOK: false,
		},
		{
			name:       "non-numeric index",
			oid:        validUUID + ".0000x",
			expectedOK: false,
		},
		{
			name:       "left segment not a uuid",
			oid:        "not-a-uuid.00000",
			expectedOK: false,
		},
		{
			name:       "foreign rgw index shard",
			oid:        ".dir.0123abcd-1234.1",
			expectedOK: false,
		},
		{
			name:       "empty",
			oid:        "",
			expectedOK: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			parsed, ok := ParseChunkOID(tc.oid)
			if ok != tc.expectedOK {
				t.Fatalf("ParseChunkOID(%q) ok = %v, want %v", tc.oid, ok, tc.expectedOK)
			}
			if ok != IsChunkOID(tc.oid) {
				t.Fatalf("IsChunkOID(%q) = %v disagrees with ParseChunkOID ok = %v", tc.oid, IsChunkOID(tc.oid), ok)
			}
			if !ok {
				return
			}
			if parsed.Index != tc.expectedIndex {
				t.Fatalf("ParseChunkOID(%q) index = %d, want %d", tc.oid, parsed.Index, tc.expectedIndex)
			}
			if parsed.ObjID != uuid.MustParse(validUUID) {
				t.Fatalf("ParseChunkOID(%q) objID = %s, want %s", tc.oid, parsed.ObjID, validUUID)
			}
		})
	}
}

func TestScanRateFromEnv(t *testing.T) {
	tests := []struct {
		name         string
		env          string
		setEnv       bool
		expectedRate int
	}{
		{name: "unset is unlimited", setEnv: false, expectedRate: 0},
		{name: "positive value honoured", env: "250", setEnv: true, expectedRate: 250},
		{name: "zero is unlimited", env: "0", setEnv: true, expectedRate: 0},
		{name: "negative coerces to unlimited", env: "-5", setEnv: true, expectedRate: 0},
		{name: "garbage coerces to unlimited", env: "fast", setEnv: true, expectedRate: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.setEnv {
				t.Setenv("STRATA_RECONCILE_SCAN_RATE", tc.env)
			} else {
				t.Setenv("STRATA_RECONCILE_SCAN_RATE", "")
			}
			if got := ScanRateFromEnv(); got != tc.expectedRate {
				t.Fatalf("ScanRateFromEnv() = %d, want %d", got, tc.expectedRate)
			}
		})
	}
}

func TestScanLimiterNilIsNoop(t *testing.T) {
	var s *ScanLimiter
	if err := s.Wait(context.Background()); err != nil {
		t.Fatalf("nil ScanLimiter.Wait = %v, want nil", err)
	}
	if NewScanLimiter(0) != nil {
		t.Fatal("NewScanLimiter(0) should be nil (unlimited)")
	}
	if NewScanLimiter(-1) != nil {
		t.Fatal("NewScanLimiter(-1) should be nil (unlimited)")
	}
}

func TestScanLimiterWaitCancels(t *testing.T) {
	s := NewScanLimiter(1) // 1/sec, burst 1
	if err := s.Wait(context.Background()); err != nil {
		t.Fatalf("first Wait should pass on burst: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Wait(ctx); err == nil {
		t.Fatal("Wait on a cancelled ctx after burst drained should error")
	}
}

// stubBackend implements data.Backend but NOT PoolEnumerator — the default
// (go-ceph-free) shape. EnumeratePool must surface the not-compiled
// sentinel rather than panic.
type stubBackend struct{}

func (stubBackend) PutChunks(context.Context, io.Reader, string) (*data.Manifest, error) {
	return nil, data.ErrRADOSNotCompiled
}
func (stubBackend) GetChunks(context.Context, *data.Manifest, int64, int64) (io.ReadCloser, error) {
	return nil, data.ErrRADOSNotCompiled
}
func (stubBackend) Delete(context.Context, *data.Manifest) error { return data.ErrRADOSNotCompiled }
func (stubBackend) Close() error                                 { return nil }

func TestEnumeratePoolNotCompiledSentinel(t *testing.T) {
	err := EnumeratePool(context.Background(), stubBackend{}, "", EnumerateOptions{Pool: "p"},
		func(PoolObject, EnumerateCursor) error { return nil })
	if !errors.Is(err, data.ErrRADOSNotCompiled) {
		t.Fatalf("EnumeratePool on non-enumerator backend = %v, want ErrRADOSNotCompiled", err)
	}
	// nil backend also takes the sentinel path (no nil-deref panic).
	if err := EnumeratePool(context.Background(), nil, "", EnumerateOptions{}, nil); !errors.Is(err, data.ErrRADOSNotCompiled) {
		t.Fatalf("EnumeratePool(nil backend) = %v, want ErrRADOSNotCompiled", err)
	}
}

// enumBackend implements PoolEnumerator so the dispatcher's happy path is
// exercised without librados.
type enumBackend struct {
	stubBackend
	got EnumerateOptions
}

func (e *enumBackend) EnumeratePool(_ context.Context, _ string, opts EnumerateOptions, visit PoolVisitor) error {
	e.got = opts
	return visit(PoolObject{OID: "obj.00000"}, EnumerateCursor(7))
}

func TestEnumeratePoolDispatches(t *testing.T) {
	be := &enumBackend{}
	var sawOID string
	var sawCursor EnumerateCursor
	err := EnumeratePool(context.Background(), be, "c1",
		EnumerateOptions{Pool: "data", ChunkOIDsOnly: true, RatePerSec: 5},
		func(o PoolObject, c EnumerateCursor) error {
			sawOID, sawCursor = o.OID, c
			return nil
		})
	if err != nil {
		t.Fatalf("EnumeratePool dispatch = %v, want nil", err)
	}
	if sawOID != "obj.00000" || sawCursor != 7 {
		t.Fatalf("visitor saw (%q, %d), want (obj.00000, 7)", sawOID, sawCursor)
	}
	if !be.got.ChunkOIDsOnly || be.got.RatePerSec != 5 || be.got.Pool != "data" {
		t.Fatalf("backend got opts %+v, want pass-through", be.got)
	}
}
