package gc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/meta/memory"
	strataotel "github.com/danchupin/strata/internal/otel"
)

// flakyBackend errors the third Delete call so the iteration carries a
// gc.delete_chunk child span with Error status — the canonical US-004
// failure-propagation assertion.
type flakyBackend struct {
	failOn int32
	calls  atomic.Int32
}

func (b *flakyBackend) PutChunks(context.Context, io.Reader, string) (*data.Manifest, error) {
	return nil, nil
}

func (b *flakyBackend) GetChunks(context.Context, *data.Manifest, int64, int64) (io.ReadCloser, error) {
	return nil, nil
}

func (b *flakyBackend) Delete(_ context.Context, _ *data.Manifest) error {
	if b.calls.Add(1) == b.failOn {
		return errors.New("boom: chunk delete failed")
	}
	return nil
}

func (b *flakyBackend) Close() error { return nil }

func TestWorkerEmitsIterationAndSubOpSpans(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	ctx := context.Background()
	store := memory.New()
	chunks := make([]data.ChunkRef, 0, 3)
	for i := range 3 {
		chunks = append(chunks, data.ChunkRef{
			Cluster: "default",
			Pool:    "hot",
			OID:     fmt.Sprintf("oid-%d", i),
			Size:    10,
		})
	}
	if err := store.EnqueueChunkDeletion(ctx, "default", chunks); err != nil {
		t.Fatalf("EnqueueChunkDeletion: %v", err)
	}

	be := &flakyBackend{failOn: 3}
	w := &Worker{
		Meta:   store,
		Data:   be,
		Region: "default",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Tracer: tp.Tracer("strata.worker.gc"),
	}
	_ = w.RunOnce(ctx)

	spans := exp.GetSpans()
	if len(spans) == 0 {
		t.Fatalf("no spans emitted")
	}

	var (
		iter    *tracetest.SpanStub
		scans   int
		deletes int
		delErr  bool
	)
	for i := range spans {
		s := &spans[i]
		switch s.Name {
		case "worker.gc.tick":
			iter = s
		case "gc.scan_partition":
			scans++
		case "gc.delete_chunk":
			deletes++
			if s.Status.Code.String() == "Error" {
				delErr = true
			}
		}
	}
	if iter == nil {
		t.Fatalf("missing worker.gc.tick iteration span; got %d spans: %v", len(spans), spanNames(spans))
	}
	if iter.Status.Code.String() != "Error" {
		t.Errorf("iteration span status = %s, want Error", iter.Status.Code)
	}
	if !hasAttr(iter.Attributes, strataotel.AttrComponentKey, "worker") {
		t.Errorf("iteration missing strata.component=worker")
	}
	if !hasAttr(iter.Attributes, strataotel.WorkerKey, "gc") {
		t.Errorf("iteration missing strata.worker=gc")
	}
	if scans < 1 {
		t.Errorf("scan_partition spans = %d, want >=1", scans)
	}
	if deletes != 3 {
		t.Errorf("delete_chunk spans = %d, want 3", deletes)
	}
	if !delErr {
		t.Errorf("expected at least one gc.delete_chunk with Error status")
	}
}

func spanNames(spans tracetest.SpanStubs) []string {
	out := make([]string, len(spans))
	for i, s := range spans {
		out[i] = s.Name
	}
	return out
}

func hasAttr(attrs []attribute.KeyValue, key, val string) bool {
	for _, kv := range attrs {
		if string(kv.Key) == key && kv.Value.AsString() == val {
			return true
		}
	}
	return false
}
