package auditexport

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gocql/gocql"
	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/data"
	datamem "github.com/danchupin/strata/internal/data/memory"
	"github.com/danchupin/strata/internal/meta"
	metamem "github.com/danchupin/strata/internal/meta/memory"
)

func newWorker(t *testing.T, store *metamem.Store, dm data.Backend, now time.Time) *Worker {
	t.Helper()
	w, err := New(Config{
		Meta:     store,
		Data:     dm,
		Bucket:   "audit-export",
		Prefix:   "exports/",
		After:    30 * 24 * time.Hour,
		Interval: time.Hour,
		Now:      func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("new worker: %v", err)
	}
	return w
}

func enqueueAged(t *testing.T, store *metamem.Store, src *meta.Bucket, when time.Time, n int) {
	t.Helper()
	ctx := context.Background()
	for i := range n {
		evt := &meta.AuditEvent{
			BucketID:  src.ID,
			Bucket:    src.Name,
			EventID:   gocql.TimeUUID().String(),
			Time:      when.Add(time.Duration(i) * time.Second),
			Principal: "alice",
			Action:    "PutObject",
			Resource:  "/" + src.Name + "/k",
			Result:    "200",
			RequestID: "req",
			SourceIP:  "10.0.0.1",
		}
		if err := store.EnqueueAudit(ctx, evt, 0); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
}

// 100 enqueued rows yield exactly 100 JSON lines in the export bucket and
// drain the source partition. Covers the headline US-046 acceptance test.
func TestRunOnceExports100RowsAndDrains(t *testing.T) {
	ctx := context.Background()
	store := metamem.New()
	dm := datamem.New()
	src, err := store.CreateBucket(ctx, "src", "alice", "STANDARD")
	if err != nil {
		t.Fatalf("create src: %v", err)
	}
	tgt, err := store.CreateBucket(ctx, "audit-export", "alice", "STANDARD")
	if err != nil {
		t.Fatalf("create export: %v", err)
	}
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	aged := now.AddDate(0, 0, -45)
	enqueueAged(t, store, src, aged, 100)

	w := newWorker(t, store, dm, now)
	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("run once: %v", err)
	}

	res, err := store.ListObjects(ctx, tgt.ID, meta.ListOptions{Prefix: "exports/", Limit: 100})
	if err != nil {
		t.Fatalf("list export: %v", err)
	}
	if len(res.Objects) != 1 {
		t.Fatalf("export objects=%d want 1", len(res.Objects))
	}
	obj := res.Objects[0]
	wantPrefix := "exports/" + aged.UTC().Format("2006-01-02") + "/src-" + src.ID.String() + ".jsonl.gz"
	if obj.Key != wantPrefix {
		t.Fatalf("export key=%q want %q", obj.Key, wantPrefix)
	}

	rc, err := dm.GetChunks(ctx, obj.Manifest, 0, obj.Size)
	if err != nil {
		t.Fatalf("get body: %v", err)
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	rows, err := DecodeJSONLinesGzip(body)
	if err != nil {
		t.Fatalf("decode export: %v", err)
	}
	if len(rows) != 100 {
		t.Fatalf("decoded rows=%d want 100", len(rows))
	}
	for i := 1; i < len(rows); i++ {
		if rows[i-1].EventID > rows[i].EventID {
			t.Fatalf("rows not sorted at %d: %s > %s", i, rows[i-1].EventID, rows[i].EventID)
		}
	}

	left, err := store.ReadAuditPartition(ctx, src.ID, aged)
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	if len(left) != 0 {
		t.Fatalf("source partition not drained: %d rows", len(left))
	}
}

// Fresh rows (younger than After) stay put; multi-day aged rows produce
// one export object per partition.
func TestRunOnceSplitsByDayAndKeepsFresh(t *testing.T) {
	ctx := context.Background()
	store := metamem.New()
	dm := datamem.New()
	src, _ := store.CreateBucket(ctx, "src", "alice", "STANDARD")
	if _, err := store.CreateBucket(ctx, "audit-export", "alice", "STANDARD"); err != nil {
		t.Fatalf("create export: %v", err)
	}
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	day1 := now.AddDate(0, 0, -40)
	day2 := now.AddDate(0, 0, -35)
	enqueueAged(t, store, src, day1, 3)
	enqueueAged(t, store, src, day2, 2)
	enqueueAged(t, store, src, now.AddDate(0, 0, -1), 1) // fresh, must not export

	w := newWorker(t, store, dm, now)
	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("run once: %v", err)
	}
	tgt, _ := store.GetBucket(ctx, "audit-export")
	res, _ := store.ListObjects(ctx, tgt.ID, meta.ListOptions{Limit: 100})
	if len(res.Objects) != 2 {
		t.Fatalf("export objects=%d want 2", len(res.Objects))
	}
	rest, _ := store.ListAudit(ctx, src.ID, 100)
	if len(rest) != 1 {
		t.Fatalf("fresh row dropped or aged kept: %d rows", len(rest))
	}
}

// Missing target bucket is a deploy/config issue, not a fatal worker
// state — it logs and skips so the operator can fix without restart.
func TestRunOnceSkipsWhenTargetMissing(t *testing.T) {
	ctx := context.Background()
	store := metamem.New()
	dm := datamem.New()
	src, _ := store.CreateBucket(ctx, "src", "alice", "STANDARD")
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	enqueueAged(t, store, src, now.AddDate(0, 0, -40), 5)
	w := newWorker(t, store, dm, now)
	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("run once: %v", err)
	}
	// Source rows untouched.
	rows, _ := store.ListAudit(ctx, src.ID, 100)
	if len(rows) != 5 {
		t.Fatalf("source mutated despite missing target: %d", len(rows))
	}
}

// IAM-scoped partitions (BucketID == uuid.Nil) export under the "iam"
// suffix so the key stays identifiable and unique.
func TestRunOnceExportsIAMPartition(t *testing.T) {
	ctx := context.Background()
	store := metamem.New()
	dm := datamem.New()
	if _, err := store.CreateBucket(ctx, "audit-export", "alice", "STANDARD"); err != nil {
		t.Fatalf("create export: %v", err)
	}
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	aged := now.AddDate(0, 0, -40)
	if err := store.EnqueueAudit(ctx, &meta.AuditEvent{
		BucketID: uuid.Nil,
		Bucket:   "-",
		EventID:  gocql.TimeUUID().String(),
		Time:     aged,
		Action:   "iam:CreateUser",
		Resource: "iam:CreateUser",
		Result:   "200",
	}, 0); err != nil {
		t.Fatalf("enqueue iam: %v", err)
	}
	w := newWorker(t, store, dm, now)
	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("run once: %v", err)
	}
	tgt, _ := store.GetBucket(ctx, "audit-export")
	res, _ := store.ListObjects(ctx, tgt.ID, meta.ListOptions{Limit: 10})
	if len(res.Objects) != 1 {
		t.Fatalf("export objects=%d want 1", len(res.Objects))
	}
	if !strings.HasSuffix(res.Objects[0].Key, "/--iam.jsonl.gz") {
		t.Fatalf("iam key suffix unexpected: %q", res.Objects[0].Key)
	}
}

func TestNewValidatesConfig(t *testing.T) {
	store := metamem.New()
	dm := datamem.New()
	cases := []struct {
		name string
		cfg  Config
	}{
		{"missing meta", Config{Data: dm, Bucket: "x"}},
		{"missing data", Config{Meta: store, Bucket: "x"}},
		{"missing bucket", Config{Meta: store, Data: dm, Bucket: "  "}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.cfg); err == nil {
				t.Fatalf("expected error")
			}
		})
	}
}
