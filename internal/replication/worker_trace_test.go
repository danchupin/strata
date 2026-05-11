package replication

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

func TestReplicatorWorkerEmitsIterationAndCopySpan(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	h := newHarness(t, func(c *Config) {
		c.Tracer = tp.Tracer("strata.worker.replicator")
	})
	o := h.putObject(t, "k", "hello")
	h.enqueue(t, &meta.ReplicationEvent{
		BucketID:            h.bucket.ID,
		Bucket:              h.bucket.Name,
		Key:                 o.Key,
		VersionID:           o.VersionID,
		EventName:           "s3:ObjectCreated:Put",
		EventTime:           time.Now().Add(-2 * time.Second).UTC(),
		RuleID:              "r1",
		DestinationBucket:   "arn:aws:s3:::dest",
		DestinationEndpoint: "peer:443",
	})

	if err := h.worker.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	spans := exp.GetSpans()
	var iter, copyObj *tracetest.SpanStub
	for i := range spans {
		s := &spans[i]
		switch s.Name {
		case "worker.replicator.tick":
			iter = s
		case "replicator.copy_object":
			copyObj = s
		}
	}
	if iter == nil {
		t.Fatalf("missing worker.replicator.tick iteration span")
	}
	if !hasAttr(iter.Attributes, strataotel.AttrComponentKey, "worker") {
		t.Errorf("iteration missing strata.component=worker; got %v", iter.Attributes)
	}
	if !hasAttr(iter.Attributes, strataotel.WorkerKey, "replicator") {
		t.Errorf("iteration missing strata.worker=replicator; got %v", iter.Attributes)
	}
	if copyObj == nil {
		t.Errorf("expected replicator.copy_object child span")
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
