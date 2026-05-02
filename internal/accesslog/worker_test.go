package accesslog

import (
	"context"
	"io"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/danchupin/strata/internal/data"
	datamem "github.com/danchupin/strata/internal/data/memory"
	"github.com/danchupin/strata/internal/meta"
	metamem "github.com/danchupin/strata/internal/meta/memory"
)

func newTestWorker(t *testing.T) (*Worker, *metamem.Store, data.Backend) {
	t.Helper()
	store := metamem.New()
	dm := datamem.New()
	w, err := New(Config{
		Meta:          store,
		Data:          dm,
		Interval:      time.Hour,
		MaxFlushBytes: 1024 * 1024,
		PollLimit:     100,
		Now:           time.Now,
	})
	if err != nil {
		t.Fatalf("new worker: %v", err)
	}
	return w, store, dm
}

func enableLogging(t *testing.T, store *metamem.Store, source, target, prefix string) {
	t.Helper()
	src, err := store.GetBucket(context.Background(), source)
	if err != nil {
		t.Fatalf("get source bucket: %v", err)
	}
	xml := `<BucketLoggingStatus xmlns="http://s3.amazonaws.com/doc/2006-03-01/">` +
		`<LoggingEnabled><TargetBucket>` + target + `</TargetBucket>` +
		`<TargetPrefix>` + prefix + `</TargetPrefix></LoggingEnabled></BucketLoggingStatus>`
	if err := store.SetBucketLogging(context.Background(), src.ID, []byte(xml)); err != nil {
		t.Fatalf("set logging: %v", err)
	}
}

func TestWorkerRoundTripWritesLogObject(t *testing.T) {
	ctx := context.Background()
	w, store, dm := newTestWorker(t)
	src, err := store.CreateBucket(ctx, "src", "alice", "STANDARD")
	if err != nil {
		t.Fatalf("create src: %v", err)
	}
	if _, err := store.CreateBucket(ctx, "logs", "alice", "STANDARD"); err != nil {
		t.Fatalf("create logs: %v", err)
	}
	enableLogging(t, store, "src", "logs", "access/")

	now := time.Date(2026, 4, 26, 10, 30, 45, 0, time.UTC)
	if err := store.EnqueueAccessLog(ctx, &meta.AccessLogEntry{
		BucketID:    src.ID,
		Bucket:      "src",
		EventID:     "evt-1",
		Time:        now,
		RequestID:   "req-1",
		Principal:   "alice",
		SourceIP:    "10.0.0.1",
		Op:          "REST.PUT.OBJECT",
		Key:         "img/cat.jpg",
		Status:      200,
		BytesSent:   1024,
		ObjectSize:  4096,
		TotalTimeMS: 12,
		Referrer:    "https://example.com/",
		UserAgent:   "aws-cli/2.0",
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("run once: %v", err)
	}

	tgt, _ := store.GetBucket(ctx, "logs")
	res, err := store.ListObjects(ctx, tgt.ID, meta.ListOptions{Prefix: "access/", Limit: 10})
	if err != nil {
		t.Fatalf("list target: %v", err)
	}
	if len(res.Objects) != 1 {
		t.Fatalf("got %d log objects want 1", len(res.Objects))
	}
	obj := res.Objects[0]
	if !strings.HasPrefix(obj.Key, "access/") {
		t.Fatalf("key prefix: %q", obj.Key)
	}
	matched, _ := regexp.MatchString(`^access/\d{4}-\d{2}-\d{2}-\d{2}-\d{2}-\d{2}-[0-9A-F]{16}$`, obj.Key)
	if !matched {
		t.Fatalf("key shape unexpected: %q", obj.Key)
	}

	rc, err := dm.GetChunks(ctx, obj.Manifest, 0, obj.Size)
	if err != nil {
		t.Fatalf("get body: %v", err)
	}
	defer rc.Close()
	body, _ := io.ReadAll(rc)
	line := strings.TrimRight(string(body), "\n")
	if !strings.Contains(line, "REST.PUT.OBJECT") || !strings.Contains(line, "img/cat.jpg") {
		t.Fatalf("log line missing expected fields: %s", line)
	}
	if !strings.HasPrefix(line, "alice src ") {
		t.Fatalf("log line should start with bucket-owner+bucket: %s", line)
	}
	if !strings.Contains(line, `"PUT /src/img/cat.jpg HTTP/1.1"`) {
		t.Fatalf("log line should carry reconstructed Request-URI: %s", line)
	}

	remaining, _ := store.ListPendingAccessLog(ctx, src.ID, 100)
	if len(remaining) != 0 {
		t.Fatalf("expected buffer drained, %d rows remain", len(remaining))
	}
}

func TestFormatLineMatchesAWSShape(t *testing.T) {
	now := time.Date(2026, 4, 26, 10, 30, 45, 0, time.UTC)
	got := FormatLine("ownerCanonical", meta.AccessLogEntry{
		Bucket:       "logs-bucket",
		Time:         now,
		RequestID:    "ABCDEF1234",
		Principal:    "arn:aws:iam::111:user/alice",
		SourceIP:     "192.0.2.1",
		Op:           "REST.GET.OBJECT",
		Key:          "puppy.jpg",
		Status:       200,
		BytesSent:    2662992,
		ObjectSize:   2662992,
		TotalTimeMS:  92,
		TurnAroundMS: 17,
		Referrer:     "http://www.example.com/webservices",
		UserAgent:    "curl/7.15.1",
	})
	parts := splitFields(got)
	want := 18
	if len(parts) != want {
		t.Fatalf("expected %d AWS fields, got %d: %q -> %v", want, len(parts), got, parts)
	}
	if parts[0] != "ownerCanonical" {
		t.Fatalf("bucket owner: %q", parts[0])
	}
	if parts[1] != "logs-bucket" {
		t.Fatalf("bucket: %q", parts[1])
	}
	if !strings.HasPrefix(parts[2], "[") || !strings.HasSuffix(parts[2], "]") {
		t.Fatalf("time field not bracketed: %q", parts[2])
	}
	if parts[3] != "192.0.2.1" {
		t.Fatalf("remote ip: %q", parts[3])
	}
	if parts[6] != "REST.GET.OBJECT" {
		t.Fatalf("operation: %q", parts[6])
	}
	if parts[7] != "puppy.jpg" {
		t.Fatalf("key: %q", parts[7])
	}
	if parts[8] != `"GET /logs-bucket/puppy.jpg HTTP/1.1"` {
		t.Fatalf("request-uri: %q", parts[8])
	}
	if parts[9] != "200" {
		t.Fatalf("http status: %q", parts[9])
	}
	if parts[10] != "-" {
		t.Fatalf("error code should be dash: %q", parts[10])
	}
	if parts[11] != "2662992" || parts[12] != "2662992" {
		t.Fatalf("byte sizes: %q %q", parts[11], parts[12])
	}
	if parts[13] != "92" || parts[14] != "17" {
		t.Fatalf("timing fields: %q %q", parts[13], parts[14])
	}
	if parts[15] != `"http://www.example.com/webservices"` {
		t.Fatalf("referrer: %q", parts[15])
	}
	if parts[16] != `"curl/7.15.1"` {
		t.Fatalf("user agent: %q", parts[16])
	}
	if parts[17] != "-" {
		t.Fatalf("version id default: %q", parts[17])
	}
}

func TestRunOnceNoLoggingNoFlush(t *testing.T) {
	ctx := context.Background()
	w, store, _ := newTestWorker(t)
	src, _ := store.CreateBucket(ctx, "src", "alice", "STANDARD")
	if _, err := store.CreateBucket(ctx, "logs", "alice", "STANDARD"); err != nil {
		t.Fatalf("create logs: %v", err)
	}
	if err := store.EnqueueAccessLog(ctx, &meta.AccessLogEntry{
		BucketID: src.ID,
		Bucket:   "src",
		EventID:  "evt-1",
		Time:     time.Now().UTC(),
		Op:       "REST.GET.OBJECT",
		Key:      "x",
		Status:   200,
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("run once: %v", err)
	}
	tgt, _ := store.GetBucket(ctx, "logs")
	res, _ := store.ListObjects(ctx, tgt.ID, meta.ListOptions{Limit: 10})
	if len(res.Objects) != 0 {
		t.Fatalf("expected no flush when logging disabled, got %d", len(res.Objects))
	}
	rows, _ := store.ListPendingAccessLog(ctx, src.ID, 100)
	if len(rows) != 1 {
		t.Fatalf("expected buffer untouched, %d rows remain", len(rows))
	}
}

func TestFlushBoundedByMaxFlushBytes(t *testing.T) {
	ctx := context.Background()
	store := metamem.New()
	dm := datamem.New()
	w, err := New(Config{
		Meta:          store,
		Data:          dm,
		Interval:      time.Hour,
		MaxFlushBytes: 256,
		PollLimit:     100,
	})
	if err != nil {
		t.Fatalf("new worker: %v", err)
	}
	src, _ := store.CreateBucket(ctx, "src", "alice", "STANDARD")
	if _, err := store.CreateBucket(ctx, "logs", "alice", "STANDARD"); err != nil {
		t.Fatalf("create logs: %v", err)
	}
	enableLogging(t, store, "src", "logs", "")

	for i := range 10 {
		if err := store.EnqueueAccessLog(ctx, &meta.AccessLogEntry{
			BucketID:  src.ID,
			Bucket:    "src",
			EventID:   "evt-" + string(rune('a'+i)),
			Time:      time.Date(2026, 4, 26, 10, 30, 45+i, 0, time.UTC),
			Principal: "alice",
			Op:        "REST.GET.OBJECT",
			Key:       "k",
			Status:    200,
		}); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("run once: %v", err)
	}
	tgt, _ := store.GetBucket(ctx, "logs")
	res, _ := store.ListObjects(ctx, tgt.ID, meta.ListOptions{Limit: 100})
	if len(res.Objects) < 2 {
		t.Fatalf("expected MaxFlushBytes to split into multiple files, got %d", len(res.Objects))
	}
}

// splitFields splits an AWS-format access log line by single spaces while
// keeping quoted and bracketed fields intact.
func splitFields(line string) []string {
	var (
		out   []string
		cur   strings.Builder
		quote byte
	)
	flush := func() {
		out = append(out, cur.String())
		cur.Reset()
	}
	for i := 0; i < len(line); i++ {
		c := line[i]
		if quote != 0 {
			cur.WriteByte(c)
			if c == quote {
				quote = 0
			}
			continue
		}
		if c == '"' {
			quote = '"'
			cur.WriteByte(c)
			continue
		}
		if c == '[' {
			quote = ']'
			cur.WriteByte(c)
			continue
		}
		if c == ' ' {
			flush()
			continue
		}
		cur.WriteByte(c)
	}
	flush()
	return out
}
