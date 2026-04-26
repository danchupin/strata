package replication

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danchupin/strata/internal/data"
	datamem "github.com/danchupin/strata/internal/data/memory"
	"github.com/danchupin/strata/internal/meta"
	metamem "github.com/danchupin/strata/internal/meta/memory"
)

type fakeDispatcher struct {
	mu        sync.Mutex
	failsLeft int
	failErr   error
	calls     int
	bodies    [][]byte
	events    []meta.ReplicationEvent
}

func (f *fakeDispatcher) Send(ctx context.Context, evt meta.ReplicationEvent, src *Source) error {
	defer src.Body.Close()
	body, _ := io.ReadAll(src.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.bodies = append(f.bodies, body)
	f.events = append(f.events, evt)
	if f.failsLeft > 0 {
		f.failsLeft--
		return f.failErr
	}
	return nil
}

type recordingMetrics struct {
	mu        sync.Mutex
	lag       map[string][]float64
	completed map[string]int
	failed    map[string]int
}

func newRecordingMetrics() *recordingMetrics {
	return &recordingMetrics{
		lag:       make(map[string][]float64),
		completed: make(map[string]int),
		failed:    make(map[string]int),
	}
}

func (m *recordingMetrics) ObserveLag(rule string, v float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lag[rule] = append(m.lag[rule], v)
}

func (m *recordingMetrics) IncCompleted(rule string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.completed[rule]++
}

func (m *recordingMetrics) IncFailed(rule string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failed[rule]++
}

type harness struct {
	store  *metamem.Store
	dataB  *datamem.Backend
	bucket *meta.Bucket
	worker *Worker
	dispC  *fakeDispatcher
	metr   *recordingMetrics
}

func newHarness(t *testing.T, opts ...func(*Config)) *harness {
	t.Helper()
	store := metamem.New()
	b, err := store.CreateBucket(context.Background(), "src", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	dataB := datamem.New()
	disp := &fakeDispatcher{}
	metr := newRecordingMetrics()
	cfg := Config{
		Meta:        store,
		Data:        dataB,
		Dispatcher:  disp,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Metrics:     metr,
		Interval:    50 * time.Millisecond,
		MaxRetries:  3,
		BackoffBase: 0,
		Backoff:     func(int) time.Duration { return 0 },
	}
	for _, o := range opts {
		o(&cfg)
	}
	w, err := New(cfg)
	if err != nil {
		t.Fatalf("new worker: %v", err)
	}
	return &harness{store: store, dataB: dataB, bucket: b, worker: w, dispC: disp, metr: metr}
}

// putObject writes a real object via data.Backend then the meta row, so the
// worker reads end-to-end through the same path the gateway uses.
func (h *harness) putObject(t *testing.T, key, body string) *meta.Object {
	t.Helper()
	m, err := h.dataB.PutChunks(context.Background(), bytes.NewReader([]byte(body)), "STANDARD")
	if err != nil {
		t.Fatalf("put chunks: %v", err)
	}
	o := &meta.Object{
		BucketID:    h.bucket.ID,
		Key:         key,
		Size:        int64(len(body)),
		ETag:        m.ETag,
		ContentType: "application/octet-stream",
		StorageClass: "STANDARD",
		Mtime:       time.Now().UTC(),
		Manifest:    m,
	}
	if err := h.store.PutObject(context.Background(), o, false); err != nil {
		t.Fatalf("put object: %v", err)
	}
	got, err := h.store.GetObject(context.Background(), h.bucket.ID, key, "")
	if err != nil {
		t.Fatalf("get object: %v", err)
	}
	return got
}

func (h *harness) enqueue(t *testing.T, evt *meta.ReplicationEvent) {
	t.Helper()
	if err := h.store.EnqueueReplication(context.Background(), evt); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
}

func TestWorkerMatchedRuleYieldsCompleted(t *testing.T) {
	h := newHarness(t)
	o := h.putObject(t, "logs/2026/04.txt", "hello world")

	evt := &meta.ReplicationEvent{
		BucketID:            h.bucket.ID,
		Bucket:              h.bucket.Name,
		Key:                 o.Key,
		VersionID:           o.VersionID,
		EventName:           "s3:ObjectCreated:Put",
		EventTime:           time.Now().Add(-2 * time.Second).UTC(),
		RuleID:              "logs",
		DestinationBucket:   "arn:aws:s3:::dest",
		DestinationEndpoint: "peer.example.com:443",
	}
	h.enqueue(t, evt)

	if err := h.worker.RunOnce(context.Background()); err != nil {
		t.Fatalf("run once: %v", err)
	}
	if h.dispC.calls != 1 {
		t.Fatalf("dispatcher calls=%d want 1", h.dispC.calls)
	}
	if string(h.dispC.bodies[0]) != "hello world" {
		t.Fatalf("body=%q", h.dispC.bodies[0])
	}

	pending, _ := h.store.ListPendingReplications(context.Background(), h.bucket.ID, 100)
	if len(pending) != 0 {
		t.Fatalf("pending after success: %d", len(pending))
	}
	updated, _ := h.store.GetObject(context.Background(), h.bucket.ID, o.Key, "")
	if updated.ReplicationStatus != ReplicationStatusCompleted {
		t.Fatalf("status=%q want COMPLETED", updated.ReplicationStatus)
	}
	if h.metr.completed["logs"] != 1 {
		t.Fatalf("completed counter=%d", h.metr.completed["logs"])
	}
	if got := h.metr.lag["logs"]; len(got) != 1 || got[0] < 1.0 {
		t.Fatalf("lag observation: %v", got)
	}
}

func TestWorkerRetriesThenMarksFailed(t *testing.T) {
	flaky := errors.New("peer 502")
	h := newHarness(t, func(c *Config) {
		c.MaxRetries = 6
	})
	h.dispC.failsLeft = 999
	h.dispC.failErr = flaky
	o := h.putObject(t, "logs/big.bin", strings.Repeat("x", 32))

	evt := &meta.ReplicationEvent{
		BucketID:            h.bucket.ID,
		Bucket:              h.bucket.Name,
		Key:                 o.Key,
		VersionID:           o.VersionID,
		EventName:           "s3:ObjectCreated:Put",
		EventTime:           time.Now().Add(-30 * time.Second).UTC(),
		RuleID:              "logs",
		DestinationBucket:   "dest",
		DestinationEndpoint: "peer:443",
	}
	h.enqueue(t, evt)

	for i := 0; i < 6; i++ {
		if err := h.worker.RunOnce(context.Background()); err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
	}
	if h.dispC.calls != 6 {
		t.Fatalf("dispatcher calls=%d want 6", h.dispC.calls)
	}
	pending, _ := h.store.ListPendingReplications(context.Background(), h.bucket.ID, 100)
	if len(pending) != 0 {
		t.Fatalf("pending after failure: %d", len(pending))
	}
	updated, _ := h.store.GetObject(context.Background(), h.bucket.ID, o.Key, "")
	if updated.ReplicationStatus != ReplicationStatusFailed {
		t.Fatalf("status=%q want FAILED", updated.ReplicationStatus)
	}
	if h.metr.failed["logs"] != 1 {
		t.Fatalf("failed counter=%d", h.metr.failed["logs"])
	}
	if got := h.metr.lag["logs"]; len(got) != 1 || got[0] < 30.0 {
		t.Fatalf("lag observation: %v", got)
	}
}

func TestWorkerRecoversAfterTransientFailures(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.MaxRetries = 6
	})
	h.dispC.failsLeft = 2
	h.dispC.failErr = errors.New("upstream 503")
	o := h.putObject(t, "logs/x", "abc")
	evt := &meta.ReplicationEvent{
		BucketID:            h.bucket.ID,
		Bucket:              h.bucket.Name,
		Key:                 o.Key,
		VersionID:           o.VersionID,
		EventTime:           time.Now().UTC(),
		RuleID:              "logs",
		DestinationBucket:   "dest",
		DestinationEndpoint: "peer:443",
	}
	h.enqueue(t, evt)
	for i := 0; i < 5; i++ {
		_ = h.worker.RunOnce(context.Background())
	}
	if h.dispC.calls != 3 {
		t.Fatalf("calls=%d want 3", h.dispC.calls)
	}
	updated, _ := h.store.GetObject(context.Background(), h.bucket.ID, o.Key, "")
	if updated.ReplicationStatus != ReplicationStatusCompleted {
		t.Fatalf("status=%q want COMPLETED", updated.ReplicationStatus)
	}
	if h.metr.failed["logs"] != 0 {
		t.Fatalf("no FAILED expected, got %d", h.metr.failed["logs"])
	}
}

func TestWorkerHonoursBackoffWindow(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	clock := &now
	h := newHarness(t, func(c *Config) {
		c.MaxRetries = 5
		c.Now = func() time.Time { return *clock }
		c.Backoff = func(int) time.Duration { return 30 * time.Second }
	})
	h.dispC.failsLeft = 999
	h.dispC.failErr = errors.New("nope")
	o := h.putObject(t, "k", "x")
	evt := &meta.ReplicationEvent{
		BucketID:            h.bucket.ID,
		Bucket:              h.bucket.Name,
		Key:                 o.Key,
		VersionID:           o.VersionID,
		EventTime:           now,
		RuleID:              "r",
		DestinationBucket:   "d",
		DestinationEndpoint: "p:443",
	}
	h.enqueue(t, evt)

	if err := h.worker.RunOnce(context.Background()); err != nil {
		t.Fatalf("first tick: %v", err)
	}
	if h.dispC.calls != 1 {
		t.Fatalf("calls after first=%d", h.dispC.calls)
	}
	*clock = clock.Add(5 * time.Second)
	if err := h.worker.RunOnce(context.Background()); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if h.dispC.calls != 1 {
		t.Fatalf("backoff not honoured, calls=%d", h.dispC.calls)
	}
	*clock = clock.Add(60 * time.Second)
	if err := h.worker.RunOnce(context.Background()); err != nil {
		t.Fatalf("third tick: %v", err)
	}
	if h.dispC.calls != 2 {
		t.Fatalf("post-backoff calls=%d want 2", h.dispC.calls)
	}
}

func TestWorkerNewRejectsMissingDeps(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatalf("expected error for missing meta")
	}
	if _, err := New(Config{Meta: metamem.New()}); err == nil {
		t.Fatalf("expected error for missing data backend")
	}
	if _, err := New(Config{Meta: metamem.New(), Data: datamem.New()}); err == nil {
		t.Fatalf("expected error for missing dispatcher")
	}
}

// compile-time assertion that the data backend signature still matches.
var _ data.Backend = (*datamem.Backend)(nil)
