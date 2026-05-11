package accesslog

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/danchupin/strata/internal/meta"
	strataotel "github.com/danchupin/strata/internal/otel"
)

func TestAccessLogWorkerEmitsIterationAndFlushSpan(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	ctx := context.Background()
	w, store, _ := newTestWorker(t)
	w.cfg.Tracer = tp.Tracer("strata.worker.access-log")

	src, err := store.CreateBucket(ctx, "src", "alice", "STANDARD")
	if err != nil {
		t.Fatalf("create src: %v", err)
	}
	if _, err := store.CreateBucket(ctx, "logs", "alice", "STANDARD"); err != nil {
		t.Fatalf("create logs: %v", err)
	}
	enableLogging(t, store, "src", "logs", "access/")
	if err := store.EnqueueAccessLog(ctx, &meta.AccessLogEntry{
		BucketID: src.ID, Bucket: "src", EventID: "evt-1",
		Time: time.Now().UTC(), Principal: "alice", Op: "REST.GET.OBJECT", Key: "k", Status: 200,
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	spans := exp.GetSpans()
	var iter, flush *tracetest.SpanStub
	for i := range spans {
		s := &spans[i]
		switch s.Name {
		case "worker.access-log.tick":
			iter = s
		case "access_log.flush_bucket":
			flush = s
		}
	}
	if iter == nil {
		t.Fatalf("missing worker.access-log.tick iteration span")
	}
	if !hasAttr(iter.Attributes, strataotel.AttrComponentKey, "worker") {
		t.Errorf("iteration missing strata.component=worker; got %v", iter.Attributes)
	}
	if !hasAttr(iter.Attributes, strataotel.WorkerKey, "access-log") {
		t.Errorf("iteration missing strata.worker=access-log; got %v", iter.Attributes)
	}
	if flush == nil {
		t.Errorf("expected access_log.flush_bucket child span")
	}
}

func hasAttr(attrs []attribute.KeyValue, key, val string) bool {
	for _, kv := range attrs {
		if string(kv.Key) == key && kv.Value.AsString() == val {
			return true
		}
	}
	return false
}
