package auditexport

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	datamem "github.com/danchupin/strata/internal/data/memory"
	metamem "github.com/danchupin/strata/internal/meta/memory"
	strataotel "github.com/danchupin/strata/internal/otel"
)

func TestAuditExportWorkerEmitsIterationAndPartitionSpan(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	ctx := context.Background()
	store := metamem.New()
	dm := datamem.New()
	src, err := store.CreateBucket(ctx, "src", "alice", "STANDARD")
	if err != nil {
		t.Fatalf("create src: %v", err)
	}
	if _, err := store.CreateBucket(ctx, "audit-export", "alice", "STANDARD"); err != nil {
		t.Fatalf("create target: %v", err)
	}
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	enqueueAged(t, store, src, now.AddDate(0, 0, -45), 3)

	w := newWorker(t, store, dm, now)
	w.cfg.Tracer = tp.Tracer("strata.worker.audit-export")
	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	spans := exp.GetSpans()
	var iter, part *tracetest.SpanStub
	for i := range spans {
		s := &spans[i]
		switch s.Name {
		case "worker.audit-export.tick":
			iter = s
		case "audit_export.export_partition":
			part = s
		}
	}
	if iter == nil {
		t.Fatalf("missing worker.audit-export.tick iteration span")
	}
	if !hasAttr(iter.Attributes, strataotel.AttrComponentKey, "worker") {
		t.Errorf("iteration missing strata.component=worker; got %v", iter.Attributes)
	}
	if !hasAttr(iter.Attributes, strataotel.WorkerKey, "audit-export") {
		t.Errorf("iteration missing strata.worker=audit-export; got %v", iter.Attributes)
	}
	if part == nil {
		t.Errorf("expected audit_export.export_partition child span")
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
