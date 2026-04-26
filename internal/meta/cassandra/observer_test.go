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
