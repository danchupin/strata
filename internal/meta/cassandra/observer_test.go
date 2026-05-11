package cassandra

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/gocql/gocql"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/danchupin/strata/internal/logging"
)

func newTestObserver(t *testing.T, threshold time.Duration) (*SlowQueryObserver, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return NewSlowQueryObserver(logger, threshold), &buf
}

func TestSlowQueryObserverLogsAboveThreshold(t *testing.T) {
	obs, buf := newTestObserver(t, 100*time.Millisecond)
	start := time.Now()
	q := gocql.ObservedQuery{
		Statement: "SELECT id, name FROM buckets WHERE name = ?",
		Start:     start,
		End:       start.Add(200 * time.Millisecond),
	}
	ctx := logging.WithRequestID(context.Background(), "req-abc")
	obs.ObserveQuery(ctx, q)

	out := buf.String()
	for _, want := range []string{
		`"level":"WARN"`,
		`"msg":"cassandra: slow query"`,
		`"request_id":"req-abc"`,
		`"table":"buckets"`,
		`"op":"SELECT"`,
		`"duration_ms":200`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("log missing %q\nfull: %s", want, out)
		}
	}
}

func TestSlowQueryObserverSilentBelowThreshold(t *testing.T) {
	obs, buf := newTestObserver(t, 100*time.Millisecond)
	start := time.Now()
	q := gocql.ObservedQuery{
		Statement: "SELECT * FROM buckets",
		Start:     start,
		End:       start.Add(50 * time.Millisecond),
	}
	obs.ObserveQuery(context.Background(), q)
	if buf.Len() != 0 {
		t.Fatalf("expected no log; got %s", buf.String())
	}
}

func TestSlowQueryObserverLogsErrorEvenIfFast(t *testing.T) {
	obs, buf := newTestObserver(t, 100*time.Millisecond)
	start := time.Now()
	q := gocql.ObservedQuery{
		Statement: "INSERT INTO objects (bucket_id, key) VALUES (?, ?)",
		Start:     start,
		End:       start.Add(5 * time.Millisecond),
		Err:       errors.New("write timeout"),
	}
	obs.ObserveQuery(context.Background(), q)

	out := buf.String()
	for _, want := range []string{
		`"level":"WARN"`,
		`"table":"objects"`,
		`"op":"INSERT"`,
		`"error":"write timeout"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("log missing %q\nfull: %s", want, out)
		}
	}
}

func TestNewSlowQueryObserverDisabled(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))
	if NewSlowQueryObserver(logger, 0) != nil {
		t.Fatal("threshold=0 must return nil observer")
	}
	if NewSlowQueryObserver(nil, time.Second) != nil {
		t.Fatal("nil logger must return nil observer")
	}
}

type captureMetrics struct {
	calls []struct {
		table, op string
		dur       time.Duration
		err       error
	}
	lwtCalls []struct{ table, bucket, shard string }
}

func (c *captureMetrics) ObserveQuery(table, op string, dur time.Duration, err error) {
	c.calls = append(c.calls, struct {
		table, op string
		dur       time.Duration
		err       error
	}{table, op, dur, err})
}

func (c *captureMetrics) IncLWTConflict(table, bucket, shard string) {
	c.lwtCalls = append(c.lwtCalls, struct{ table, bucket, shard string }{table, bucket, shard})
}

func TestQueryObserverRecordsMetricForEveryQuery(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	m := &captureMetrics{}
	obs := NewQueryObserver(logger, 100*time.Millisecond, m, nil)
	if obs == nil {
		t.Fatal("expected non-nil observer with metrics")
	}

	start := time.Now()
	// Fast query: not logged but still recorded by metrics.
	obs.ObserveQuery(context.Background(), gocql.ObservedQuery{
		Statement: "SELECT * FROM buckets",
		Start:     start,
		End:       start.Add(10 * time.Millisecond),
	})
	if len(m.calls) != 1 {
		t.Fatalf("metrics should record fast queries; got %d calls", len(m.calls))
	}
	if m.calls[0].table != "buckets" || m.calls[0].op != "SELECT" {
		t.Fatalf("unexpected (table,op): (%q,%q)", m.calls[0].table, m.calls[0].op)
	}
	if m.calls[0].dur != 10*time.Millisecond {
		t.Fatalf("expected dur=10ms, got %v", m.calls[0].dur)
	}
	if buf.Len() != 0 {
		t.Fatalf("fast query should not log: %s", buf.String())
	}

	// Slow query: metrics + log.
	obs.ObserveQuery(context.Background(), gocql.ObservedQuery{
		Statement: "INSERT INTO objects (bucket_id, key) VALUES (?, ?)",
		Start:     start,
		End:       start.Add(250 * time.Millisecond),
	})
	if len(m.calls) != 2 {
		t.Fatalf("metrics should record slow queries; got %d calls", len(m.calls))
	}
	if !strings.Contains(buf.String(), `"msg":"cassandra: slow query"`) {
		t.Fatalf("slow query should log; got %s", buf.String())
	}
}

func TestQueryObserverMetricsOnlyNoLogger(t *testing.T) {
	m := &captureMetrics{}
	obs := NewQueryObserver(nil, 0, m, nil)
	if obs == nil {
		t.Fatal("metrics-only observer must be non-nil")
	}
	start := time.Now()
	obs.ObserveQuery(context.Background(), gocql.ObservedQuery{
		Statement: "DELETE FROM multipart_uploads WHERE upload_id = ?",
		Start:     start,
		End:       start.Add(2 * time.Millisecond),
	})
	if len(m.calls) != 1 || m.calls[0].table != "multipart_uploads" || m.calls[0].op != "DELETE" {
		t.Fatalf("unexpected metrics calls: %+v", m.calls)
	}
}

func TestQueryObserverDisabledWhenAllMissing(t *testing.T) {
	if NewQueryObserver(nil, 0, nil, nil) != nil {
		t.Fatal("nil logger AND nil metrics AND nil tracer must return nil observer")
	}
}

func TestQueryObserverPassesErrorToMetrics(t *testing.T) {
	m := &captureMetrics{}
	obs := NewQueryObserver(nil, 0, m, nil)
	start := time.Now()
	obs.ObserveQuery(context.Background(), gocql.ObservedQuery{
		Statement: "UPDATE objects SET deleted = ?",
		Start:     start,
		End:       start.Add(time.Millisecond),
		Err:       errors.New("write timeout"),
	})
	if len(m.calls) != 1 {
		t.Fatalf("expected 1 metrics call, got %d", len(m.calls))
	}
	if m.calls[0].err == nil || m.calls[0].err.Error() != "write timeout" {
		t.Fatalf("error not forwarded: %v", m.calls[0].err)
	}
}

func TestSlowMSFromEnv(t *testing.T) {
	cases := []struct {
		set   bool
		value string
		want  int
	}{
		{set: false, want: 100},
		{set: true, value: "", want: 100},
		{set: true, value: "0", want: 0},
		{set: true, value: "250", want: 250},
		{set: true, value: "garbage", want: 100},
		{set: true, value: "-5", want: 100},
	}
	for _, tc := range cases {
		t.Run(tc.value, func(t *testing.T) {
			if tc.set {
				t.Setenv(EnvSlowQueryMS, tc.value)
			} else {
				_ = ""
			}
			if got := SlowMSFromEnv(); got != tc.want {
				t.Fatalf("got %d want %d", got, tc.want)
			}
		})
	}
}

func TestParseStatement(t *testing.T) {
	cases := []struct {
		stmt      string
		wantTable string
		wantOp    string
	}{
		{"SELECT id FROM buckets WHERE name = ?", "buckets", "SELECT"},
		{"INSERT INTO objects (bucket_id, key) VALUES (?, ?)", "objects", "INSERT"},
		{"UPDATE buckets SET versioning = ? WHERE name = ? IF EXISTS", "buckets", "UPDATE"},
		{"DELETE FROM multipart_uploads WHERE upload_id = ?", "multipart_uploads", "DELETE"},
		{"CREATE TABLE IF NOT EXISTS access_log_buffer (...)", "access_log_buffer", "CREATE"},
		{"ALTER TABLE objects ADD sse_key blob", "objects", "ALTER"},
		{"  ", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.stmt, func(t *testing.T) {
			table, op := parseStatement(tc.stmt)
			if table != tc.wantTable || op != tc.wantOp {
				t.Fatalf("got (%q,%q) want (%q,%q)", table, op, tc.wantTable, tc.wantOp)
			}
		})
	}
}

func TestTruncateStatementCollapsesWhitespace(t *testing.T) {
	in := "SELECT  id\n\tFROM   buckets"
	want := "SELECT id FROM buckets"
	if got := truncateStatement(in); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestQueryObserverEmitsSpan(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSyncer(exp),
	)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	tracer := tp.Tracer("strata.meta.cassandra")
	obs := NewQueryObserver(nil, 0, nil, tracer)
	if obs == nil {
		t.Fatal("expected non-nil observer with tracer")
	}

	parentTracer := tp.Tracer("strata.gateway")
	ctx, parent := parentTracer.Start(context.Background(), "PUT /bkt/key")

	start := time.Now()
	obs.ObserveQuery(ctx, gocql.ObservedQuery{
		Statement: "INSERT INTO objects (bucket_id, key) VALUES (?, ?)",
		Start:     start,
		End:       start.Add(7 * time.Millisecond),
	})
	parent.End()

	spans := exp.GetSpans()
	if len(spans) != 2 {
		t.Fatalf("want 2 spans (cassandra child + parent root), got %d", len(spans))
	}
	var child *tracetest.SpanStub
	for i := range spans {
		if spans[i].Name == "meta.cassandra.objects.INSERT" {
			child = &spans[i]
			break
		}
	}
	if child == nil {
		names := make([]string, len(spans))
		for i, s := range spans {
			names[i] = s.Name
		}
		t.Fatalf("missing cassandra child span; got names=%v", names)
	}
	if child.Parent.SpanID() != parent.SpanContext().SpanID() {
		t.Errorf("child parent=%s want %s", child.Parent.SpanID(), parent.SpanContext().SpanID())
	}
	if child.SpanKind.String() != "client" {
		t.Errorf("span kind=%s want client", child.SpanKind)
	}
	wantAttrs := map[string]string{
		"db.system":          "cassandra",
		"db.cassandra.table": "objects",
		"db.operation":       "INSERT",
		"strata.component":   "gateway",
	}
	for k, v := range wantAttrs {
		found := false
		for _, kv := range child.Attributes {
			if string(kv.Key) == k && kv.Value.AsString() == v {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing attr %s=%s; got %v", k, v, child.Attributes)
		}
	}
}

func TestObserverRecordLWTConflictFansOutToMetrics(t *testing.T) {
	m := &captureMetrics{}
	obs := NewQueryObserver(nil, 0, m, nil)
	if obs == nil {
		t.Fatal("metrics-only observer must be non-nil")
	}
	obs.RecordLWTConflict(context.Background(), "objects", "bkt-alpha", "17")
	obs.RecordLWTConflict(context.Background(), "objects", "bkt-beta", "3")

	if len(m.lwtCalls) != 2 {
		t.Fatalf("got %d LWT calls; want 2", len(m.lwtCalls))
	}
	if m.lwtCalls[0].table != "objects" || m.lwtCalls[0].bucket != "bkt-alpha" || m.lwtCalls[0].shard != "17" {
		t.Errorf("first call=%+v", m.lwtCalls[0])
	}
	if m.lwtCalls[1].bucket != "bkt-beta" || m.lwtCalls[1].shard != "3" {
		t.Errorf("second call=%+v", m.lwtCalls[1])
	}
}

func TestObserverRecordLWTConflictNoSinkIsNoop(t *testing.T) {
	// Logger-only observer: no Metrics sink. RecordLWTConflict must not panic.
	obs, _ := newTestObserver(t, 100*time.Millisecond)
	obs.RecordLWTConflict(context.Background(), "objects", "bkt", "0")

	// nil receiver is also a no-op.
	var nilObs *SlowQueryObserver
	nilObs.RecordLWTConflict(context.Background(), "objects", "bkt", "0")
}

func TestQueryObserverErrorSpanFlipsStatus(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSyncer(exp),
	)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	obs := NewQueryObserver(nil, 0, nil, tp.Tracer("t"))
	start := time.Now()
	obs.ObserveQuery(context.Background(), gocql.ObservedQuery{
		Statement: "DELETE FROM objects WHERE bucket_id=?",
		Start:     start,
		End:       start.Add(time.Millisecond),
		Err:       errors.New("write timeout"),
	})

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("want 1 span, got %d", len(spans))
	}
	if spans[0].Status.Code.String() != "Error" {
		t.Errorf("status=%s want Error", spans[0].Status.Code)
	}
}
