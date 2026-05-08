package storetest

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/meta"
)

// BenchOptions tunes the meta.Store benchmark harness. Backends consume the
// harness via Bench(b, newStore, opts); zero values fall through to
// BenchDefaults so each backend's benchmark file stays terse.
type BenchOptions struct {
	// Concurrency is the number of goroutines each parallel sub-benchmark
	// spawns. Acceptance criterion (US-018): 50 concurrent writers.
	Concurrency int
	// ListSize is the number of objects pre-populated before the
	// ListObjects benchmark runs. Setup happens outside the timed loop.
	// Acceptance criterion (US-018): 100k-object bucket, page=1000.
	ListSize int
	// PageSize is the ListObjects page size used inside the timed loop.
	PageSize int
	// AuditPreload is the number of audit rows pre-populated before the
	// AuditSweepPartition benchmark runs. The sweep bench times the cost
	// of enumerating + dropping one fully-aged partition.
	AuditPreload int
}

// BenchDefaults captures the published harness defaults documented in
// docs/site/content/architecture/benchmarks/meta-backend-comparison.md. Override per-field on a
// laptop-sized run; leave at default for the headline numbers.
var BenchDefaults = BenchOptions{
	Concurrency:  50,
	ListSize:     100_000,
	PageSize:     1_000,
	AuditPreload: 10_000,
}

// Bench drives every hot-path sub-benchmark against newStore.
//
// Sub-benchmarks (acceptance criterion mapping for US-018):
//
//   - CreateBucket            — pessimistic create-if-not-exists hot path
//   - GetObject               — single Get / range-scan-with-limit-1 latency
//   - ListObjects_100k        — full-page list against a pre-populated bucket
//   - CompleteMultipartUpload — LWT-equivalent status flip
//   - GetIAMAccessKey         — SigV4 verifier hot path
//   - AuditAppend             — append-only audit row insert
//   - AuditSweepPartition     — list + delete of one fully-aged partition
//   - RangeScanObjects_100k   — only when the backend implements
//     meta.RangeScanStore (memory + tikv); skipped on Cassandra.
//
// Run with `go test -bench=. -benchtime=5m ./internal/meta/storetest/...`
// for the documented measurement window. -short shrinks ListSize and
// AuditPreload so smoke runs finish in seconds.
func Bench(b *testing.B, newStore func() meta.Store, opts BenchOptions) {
	b.Helper()
	if opts.Concurrency <= 0 {
		opts.Concurrency = BenchDefaults.Concurrency
	}
	if opts.ListSize <= 0 {
		opts.ListSize = BenchDefaults.ListSize
	}
	if opts.PageSize <= 0 {
		opts.PageSize = BenchDefaults.PageSize
	}
	if opts.AuditPreload <= 0 {
		opts.AuditPreload = BenchDefaults.AuditPreload
	}
	if testing.Short() {
		// -short: shrink the heavy fixtures so a smoke run finishes
		// in seconds. Headline numbers MUST run without -short.
		if opts.ListSize > 1_000 {
			opts.ListSize = 1_000
		}
		if opts.AuditPreload > 1_000 {
			opts.AuditPreload = 1_000
		}
	}

	b.Run("CreateBucket", func(b *testing.B) {
		s := newStore()
		defer s.Close()
		benchCreateBucket(b, s, opts.Concurrency)
	})
	b.Run("GetObject", func(b *testing.B) {
		s := newStore()
		defer s.Close()
		benchGetObject(b, s, opts.Concurrency)
	})
	b.Run("ListObjects_100k", func(b *testing.B) {
		s := newStore()
		defer s.Close()
		benchListObjects(b, s, opts)
	})
	b.Run("CompleteMultipartUpload", func(b *testing.B) {
		s := newStore()
		defer s.Close()
		benchCompleteMultipart(b, s, opts.Concurrency)
	})
	b.Run("GetIAMAccessKey", func(b *testing.B) {
		s := newStore()
		defer s.Close()
		benchGetIAMAccessKey(b, s, opts.Concurrency)
	})
	b.Run("AuditAppend", func(b *testing.B) {
		s := newStore()
		defer s.Close()
		benchAuditAppend(b, s, opts.Concurrency)
	})
	b.Run("AuditSweepPartition", func(b *testing.B) {
		s := newStore()
		defer s.Close()
		benchAuditSweep(b, s, opts)
	})
	b.Run("RangeScanObjects_100k", func(b *testing.B) {
		s := newStore()
		defer s.Close()
		rs, ok := s.(meta.RangeScanStore)
		if !ok {
			b.Skip("backend does not implement meta.RangeScanStore — see US-012")
		}
		benchRangeScanObjects(b, rs, s, opts)
	})
}

// runParallel spawns conc workers that race to consume b.N units of work
// via an atomic counter. Mirrors b.RunParallel's shape but pins the
// goroutine count to conc instead of GOMAXPROCS × p — needed to honour
// the "50 concurrent writers" acceptance criterion regardless of host.
func runParallel(b *testing.B, conc int, work func(ctx context.Context, i int) error) {
	b.Helper()
	var counter atomic.Int64
	var wg sync.WaitGroup
	wg.Add(conc)
	b.ResetTimer()
	for range conc {
		go func() {
			defer wg.Done()
			ctx := context.Background()
			for {
				i := counter.Add(1) - 1
				if i >= int64(b.N) {
					return
				}
				if err := work(ctx, int(i)); err != nil {
					b.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	b.StopTimer()
}

func benchCreateBucket(b *testing.B, s meta.Store, conc int) {
	prefix := fmt.Sprintf("bk-%d-", time.Now().UnixNano())
	runParallel(b, conc, func(ctx context.Context, i int) error {
		name := fmt.Sprintf("%s%010d", prefix, i)
		_, err := s.CreateBucket(ctx, name, "owner", "STANDARD")
		return err
	})
}

func benchGetObject(b *testing.B, s meta.Store, conc int) {
	ctx := context.Background()
	bkt, err := s.CreateBucket(ctx, "bench-get", "owner", "STANDARD")
	if err != nil {
		b.Fatalf("create bucket: %v", err)
	}
	o := &meta.Object{
		BucketID:     bkt.ID,
		Key:          "obj",
		StorageClass: "STANDARD",
		ETag:         "deadbeef",
		Size:         1,
		Mtime:        time.Now().UTC(),
		Manifest:     &data.Manifest{Class: "STANDARD", Size: 1},
	}
	if err := s.PutObject(ctx, o, false); err != nil {
		b.Fatalf("put: %v", err)
	}
	runParallel(b, conc, func(ctx context.Context, _ int) error {
		_, err := s.GetObject(ctx, bkt.ID, "obj", "")
		return err
	})
}

func benchListObjects(b *testing.B, s meta.Store, opts BenchOptions) {
	ctx := context.Background()
	bkt, err := s.CreateBucket(ctx, "bench-list", "owner", "STANDARD")
	if err != nil {
		b.Fatalf("create bucket: %v", err)
	}
	for i := range opts.ListSize {
		o := &meta.Object{
			BucketID:     bkt.ID,
			Key:          fmt.Sprintf("k/%010d", i),
			StorageClass: "STANDARD",
			ETag:         "e",
			Size:         1,
			Mtime:        time.Now().UTC(),
			Manifest:     &data.Manifest{Class: "STANDARD", Size: 1},
		}
		if err := s.PutObject(ctx, o, false); err != nil {
			b.Fatalf("put %d: %v", i, err)
		}
	}
	listOpts := meta.ListOptions{Prefix: "k/", Limit: opts.PageSize}
	runParallel(b, opts.Concurrency, func(ctx context.Context, _ int) error {
		_, err := s.ListObjects(ctx, bkt.ID, listOpts)
		return err
	})
}

func benchCompleteMultipart(b *testing.B, s meta.Store, conc int) {
	ctx := context.Background()
	bkt, err := s.CreateBucket(ctx, "bench-mpu", "owner", "STANDARD")
	if err != nil {
		b.Fatalf("create bucket: %v", err)
	}
	runParallel(b, conc, func(ctx context.Context, i int) error {
		uploadID := uuid.New().String()
		key := fmt.Sprintf("mpu-%d", i)
		mu := &meta.MultipartUpload{
			BucketID:     bkt.ID,
			UploadID:     uploadID,
			Key:          key,
			StorageClass: "STANDARD",
			InitiatedAt:  time.Now().UTC(),
			Status:       "uploading",
		}
		if err := s.CreateMultipartUpload(ctx, mu); err != nil {
			return fmt.Errorf("init: %w", err)
		}
		part := &meta.MultipartPart{
			PartNumber: 1,
			ETag:       "deadbeef",
			Size:       1,
			Manifest:   &data.Manifest{Class: "STANDARD", Size: 1},
		}
		if err := s.SavePart(ctx, bkt.ID, uploadID, part); err != nil {
			return fmt.Errorf("save: %w", err)
		}
		obj := &meta.Object{
			BucketID:     bkt.ID,
			Key:          key,
			StorageClass: "STANDARD",
			ETag:         `"deadbeef-1"`,
			Size:         1,
			Mtime:        time.Now().UTC(),
			Manifest:     &data.Manifest{Class: "STANDARD", Size: 1},
		}
		_, err := s.CompleteMultipartUpload(ctx, obj, uploadID, []meta.CompletePart{
			{PartNumber: 1, ETag: "deadbeef"},
		}, false)
		return err
	})
}

func benchGetIAMAccessKey(b *testing.B, s meta.Store, conc int) {
	ctx := context.Background()
	user := &meta.IAMUser{UserName: "bench", UserID: uuid.New().String(), CreatedAt: time.Now().UTC()}
	if err := s.CreateIAMUser(ctx, user); err != nil {
		b.Fatalf("create user: %v", err)
	}
	ak := &meta.IAMAccessKey{
		AccessKeyID:     "AKIABENCH00000000000",
		SecretAccessKey: "secret",
		UserName:        user.UserName,
		CreatedAt:       time.Now().UTC(),
	}
	if err := s.CreateIAMAccessKey(ctx, ak); err != nil {
		b.Fatalf("create access key: %v", err)
	}
	runParallel(b, conc, func(ctx context.Context, _ int) error {
		_, err := s.GetIAMAccessKey(ctx, ak.AccessKeyID)
		return err
	})
}

func benchAuditAppend(b *testing.B, s meta.Store, conc int) {
	ctx := context.Background()
	bkt, err := s.CreateBucket(ctx, "bench-audit", "owner", "STANDARD")
	if err != nil {
		b.Fatalf("create bucket: %v", err)
	}
	runParallel(b, conc, func(ctx context.Context, i int) error {
		evt := &meta.AuditEvent{
			BucketID:  bkt.ID,
			Bucket:    bkt.Name,
			EventID:   fmt.Sprintf("evt-%d", i),
			Time:      time.Now().UTC(),
			Principal: "owner",
			Action:    "PutObject",
			Resource:  fmt.Sprintf("bench-audit/k-%d", i),
			Result:    "ok",
			RequestID: fmt.Sprintf("req-%d", i),
			SourceIP:  "127.0.0.1",
		}
		return s.EnqueueAudit(ctx, evt, 30*24*time.Hour)
	})
}

func benchAuditSweep(b *testing.B, s meta.Store, opts BenchOptions) {
	ctx := context.Background()
	bkt, err := s.CreateBucket(ctx, "bench-sweep", "owner", "STANDARD")
	if err != nil {
		b.Fatalf("create bucket: %v", err)
	}
	// Pre-populate aged audit rows. Time 60 days ago so
	// ListAuditPartitionsBefore(now-30d) sees them as fully aged.
	old := time.Now().UTC().Add(-60 * 24 * time.Hour)
	for i := range opts.AuditPreload {
		evt := &meta.AuditEvent{
			BucketID:  bkt.ID,
			Bucket:    bkt.Name,
			EventID:   fmt.Sprintf("aged-%010d", i),
			Time:      old,
			Principal: "owner",
			Action:    "PutObject",
			Resource:  fmt.Sprintf("bench-sweep/k-%d", i),
			Result:    "ok",
			RequestID: fmt.Sprintf("req-%d", i),
		}
		// Long TTL so the entries do not expire mid-bench; the sweep
		// path under test is the partition-scan + delete shape, not
		// the per-row TTL prune.
		if err := s.EnqueueAudit(ctx, evt, 365*24*time.Hour); err != nil {
			b.Fatalf("enqueue: %v", err)
		}
	}
	cutoff := time.Now().UTC().Add(-30 * 24 * time.Hour)
	b.ResetTimer()
	for i := range b.N {
		parts, err := s.ListAuditPartitionsBefore(ctx, cutoff)
		if err != nil {
			b.Fatalf("list partitions: %v", err)
		}
		for _, p := range parts {
			if _, err := s.ReadAuditPartition(ctx, p.BucketID, p.Day); err != nil {
				b.Fatalf("read partition: %v", err)
			}
			if err := s.DeleteAuditPartition(ctx, p.BucketID, p.Day); err != nil {
				b.Fatalf("delete partition: %v", err)
			}
		}
		// Repopulate so the next iteration has work — accounted in
		// the timed loop on purpose: a real sweeper amortises over
		// fresh data, so an empty-store iteration would lie.
		b.StopTimer()
		for j := range opts.AuditPreload {
			evt := &meta.AuditEvent{
				BucketID: bkt.ID,
				Bucket:   bkt.Name,
				EventID:  fmt.Sprintf("aged-%d-%010d", i, j),
				Time:     old,
			}
			if err := s.EnqueueAudit(ctx, evt, 365*24*time.Hour); err != nil {
				b.Fatalf("enqueue: %v", err)
			}
		}
		b.StartTimer()
	}
}

func benchRangeScanObjects(b *testing.B, rs meta.RangeScanStore, s meta.Store, opts BenchOptions) {
	ctx := context.Background()
	bkt, err := s.CreateBucket(ctx, "bench-rangescan", "owner", "STANDARD")
	if err != nil {
		b.Fatalf("create bucket: %v", err)
	}
	for i := range opts.ListSize {
		o := &meta.Object{
			BucketID:     bkt.ID,
			Key:          fmt.Sprintf("k/%010d", i),
			StorageClass: "STANDARD",
			ETag:         "e",
			Size:         1,
			Mtime:        time.Now().UTC(),
			Manifest:     &data.Manifest{Class: "STANDARD", Size: 1},
		}
		if err := s.PutObject(ctx, o, false); err != nil {
			b.Fatalf("put: %v", err)
		}
	}
	listOpts := meta.ListOptions{Prefix: "k/", Limit: opts.PageSize}
	runParallel(b, opts.Concurrency, func(ctx context.Context, _ int) error {
		_, err := rs.ScanObjects(ctx, bkt.ID, listOpts)
		return err
	})
}
