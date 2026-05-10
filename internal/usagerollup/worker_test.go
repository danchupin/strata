package usagerollup

import (
	"context"
	"testing"
	"time"

	"github.com/danchupin/strata/internal/meta"
	metamem "github.com/danchupin/strata/internal/meta/memory"
)

func TestRunOnceWritesYesterdayRowFromBucketStats(t *testing.T) {
	store := metamem.New()
	ctx := context.Background()
	b, err := store.CreateBucket(ctx, "rollup", "alice", "STANDARD")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := store.BumpBucketStats(ctx, b.ID, 1024, 3); err != nil {
		t.Fatalf("bump: %v", err)
	}

	now := time.Date(2026, 5, 10, 0, 5, 0, 0, time.UTC)
	w, err := New(Config{Meta: store, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	stats, err := w.RunOnce(ctx, now)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if stats.BucketsScanned != 1 || stats.RowsWritten != 1 {
		t.Fatalf("stats: %+v", stats)
	}

	yesterday := time.Date(2026, 5, 9, 0, 0, 0, 0, time.UTC)
	rows, err := store.ListUsageAggregates(ctx, b.ID, "STANDARD", yesterday, yesterday.AddDate(0, 0, 1))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows=%d (%+v)", len(rows), rows)
	}
	r := rows[0]
	wantBytesSeconds := int64(1024) * 86400
	if r.ByteSeconds != wantBytesSeconds {
		t.Fatalf("byte_seconds: got %d want %d", r.ByteSeconds, wantBytesSeconds)
	}
	if r.ObjectCountAvg != 3 || r.ObjectCountMax != 3 {
		t.Fatalf("object counts: avg=%d max=%d want both 3", r.ObjectCountAvg, r.ObjectCountMax)
	}
	if !r.Day.Equal(yesterday) {
		t.Fatalf("day: got %v want %v", r.Day, yesterday)
	}
}

func TestRunOnceEmptyBucketStatsWritesZeroRow(t *testing.T) {
	store := metamem.New()
	ctx := context.Background()
	b, err := store.CreateBucket(ctx, "empty", "alice", "STANDARD")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	now := time.Date(2026, 5, 10, 1, 0, 0, 0, time.UTC)
	w, err := New(Config{Meta: store, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if _, err := w.RunOnce(ctx, now); err != nil {
		t.Fatalf("run: %v", err)
	}
	yesterday := time.Date(2026, 5, 9, 0, 0, 0, 0, time.UTC)
	rows, err := store.ListUsageAggregates(ctx, b.ID, "STANDARD", yesterday, yesterday.AddDate(0, 0, 1))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows=%d", len(rows))
	}
	if rows[0].ByteSeconds != 0 || rows[0].ObjectCountAvg != 0 {
		t.Fatalf("expected zero stats: %+v", rows[0])
	}
}

func TestRunOnceUsesBucketDefaultClass(t *testing.T) {
	store := metamem.New()
	ctx := context.Background()
	b, err := store.CreateBucket(ctx, "glacier", "alice", "GLACIER")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := store.BumpBucketStats(ctx, b.ID, 100, 1); err != nil {
		t.Fatalf("bump: %v", err)
	}
	now := time.Date(2026, 5, 10, 0, 0, 5, 0, time.UTC)
	w, err := New(Config{Meta: store, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if _, err := w.RunOnce(ctx, now); err != nil {
		t.Fatalf("run: %v", err)
	}
	yesterday := time.Date(2026, 5, 9, 0, 0, 0, 0, time.UTC)
	rows, err := store.ListUsageAggregates(ctx, b.ID, "GLACIER", yesterday, yesterday.AddDate(0, 0, 1))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows=%d (%+v)", len(rows), rows)
	}
}

func TestRunOnceFanoutAcrossBuckets(t *testing.T) {
	store := metamem.New()
	ctx := context.Background()
	b1, _ := store.CreateBucket(ctx, "b1", "alice", "STANDARD")
	b2, _ := store.CreateBucket(ctx, "b2", "alice", "STANDARD")
	if _, err := store.BumpBucketStats(ctx, b1.ID, 1, 1); err != nil {
		t.Fatalf("bump b1: %v", err)
	}
	if _, err := store.BumpBucketStats(ctx, b2.ID, 2, 2); err != nil {
		t.Fatalf("bump b2: %v", err)
	}
	now := time.Date(2026, 5, 10, 0, 30, 0, 0, time.UTC)
	w, _ := New(Config{Meta: store, Now: func() time.Time { return now }})
	stats, err := w.RunOnce(ctx, now)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if stats.BucketsScanned != 2 || stats.RowsWritten != 2 {
		t.Fatalf("stats: %+v", stats)
	}
	user, err := store.ListUserUsage(ctx, "alice", time.Date(2026, 5, 9, 0, 0, 0, 0, time.UTC), time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("user usage: %v", err)
	}
	var total int64
	for _, r := range user {
		total += r.ByteSeconds
	}
	wantTotal := int64(3) * 86400
	if total != wantTotal {
		t.Fatalf("user total: got %d want %d (%+v)", total, wantTotal, user)
	}
}

func TestNextFireTodayBeforeAt(t *testing.T) {
	w, _ := New(Config{Meta: metamem.New(), At: "00:00", Now: func() time.Time {
		return time.Date(2026, 5, 10, 23, 59, 0, 0, time.UTC)
	}})
	got := w.nextFire(time.Date(2026, 5, 10, 23, 59, 0, 0, time.UTC))
	want := time.Date(2026, 5, 11, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("nextFire: got %v want %v", got, want)
	}
}

func TestNextFireTodayAfterAt(t *testing.T) {
	w, _ := New(Config{Meta: metamem.New(), At: "06:30", Now: func() time.Time {
		return time.Date(2026, 5, 10, 5, 0, 0, 0, time.UTC)
	}})
	got := w.nextFire(time.Date(2026, 5, 10, 5, 0, 0, 0, time.UTC))
	want := time.Date(2026, 5, 10, 6, 30, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("nextFire: got %v want %v", got, want)
	}
}

func TestNewRequiresMeta(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("want error for missing meta")
	}
}

func TestNewRejectsBadAt(t *testing.T) {
	if _, err := New(Config{Meta: metamem.New(), At: "25:99"}); err == nil {
		t.Fatal("want error for invalid clock")
	}
}

func TestRunCtxCancelExits(t *testing.T) {
	store := metamem.New()
	w, _ := New(Config{Meta: store, Interval: 50 * time.Millisecond, At: "00:00"})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit within 2s of ctx cancel")
	}
}

// Sanity: ensure the worker contract holds against a fresh-empty store.
func TestRunOnceEmptyMeta(t *testing.T) {
	store := metamem.New()
	w, _ := New(Config{Meta: store})
	stats, err := w.RunOnce(context.Background(), time.Now().UTC())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if stats.BucketsScanned != 0 || stats.RowsWritten != 0 {
		t.Fatalf("stats: %+v", stats)
	}
	_ = meta.UsageAggregate{}
}
