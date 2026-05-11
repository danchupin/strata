package inventory_test

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	datamem "github.com/danchupin/strata/internal/data/memory"
	"github.com/danchupin/strata/internal/inventory"
	"github.com/danchupin/strata/internal/meta"
	metamem "github.com/danchupin/strata/internal/meta/memory"
	strataotel "github.com/danchupin/strata/internal/otel"
)

func TestInventoryWorkerEmitsIterationAndScanSpan(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	mem := metamem.New()
	d := datamem.New()
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	w, err := inventory.New(inventory.Config{
		Meta:   mem,
		Data:   d,
		Now:    func() time.Time { return now },
		Tracer: tp.Tracer("strata.worker.inventory"),
	})
	if err != nil {
		t.Fatalf("worker: %v", err)
	}

	ctx := context.Background()
	src, err := mem.CreateBucket(ctx, "src", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("create src: %v", err)
	}
	if _, err := mem.CreateBucket(ctx, "dest", "owner", "STANDARD"); err != nil {
		t.Fatalf("create dest: %v", err)
	}
	if err := mem.SetBucketInventoryConfig(ctx, src.ID, "list1", []byte(inventoryXML)); err != nil {
		t.Fatalf("set inventory: %v", err)
	}

	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	spans := exp.GetSpans()
	var iter, scan *tracetest.SpanStub
	for i := range spans {
		s := &spans[i]
		switch s.Name {
		case "worker.inventory.tick":
			iter = s
		case "inventory.scan_bucket":
			scan = s
		}
	}
	if iter == nil {
		t.Fatalf("missing worker.inventory.tick iteration span")
	}
	if !hasAttr(iter.Attributes, strataotel.AttrComponentKey, "worker") {
		t.Errorf("iteration missing strata.component=worker; got %v", iter.Attributes)
	}
	if !hasAttr(iter.Attributes, strataotel.WorkerKey, "inventory") {
		t.Errorf("iteration missing strata.worker=inventory; got %v", iter.Attributes)
	}
	if scan == nil {
		t.Errorf("expected inventory.scan_bucket child span")
	}

	// Silence unused-import warning if a future test edit drops meta usage.
	_ = meta.NullVersionID
}

func hasAttr(attrs []attribute.KeyValue, key, val string) bool {
	for _, kv := range attrs {
		if string(kv.Key) == key && kv.Value.AsString() == val {
			return true
		}
	}
	return false
}
