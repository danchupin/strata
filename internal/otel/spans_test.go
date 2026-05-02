package otel_test

import (
	"context"
	"testing"
	"time"

	"github.com/gocql/gocql"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	rados "github.com/danchupin/strata/internal/data/rados"
	cassandra "github.com/danchupin/strata/internal/meta/cassandra"
	strataotel "github.com/danchupin/strata/internal/otel"
)

// TestSpanHierarchyForSmokePut wires the cassandra QueryObserver and the
// RADOS ObserveOp helper into a single Provider with an in-memory exporter
// and asserts the resulting trace contains a root server span plus at least
// one cassandra child and one RADOS child, parented to the root. This covers
// the US-033 acceptance criterion that the smoke PUT path produces the
// expected span hierarchy via the in-memory exporter.
func TestSpanHierarchyForSmokePut(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	prev := otel.GetTracerProvider()
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	p, err := strataotel.InitWithConfig(context.Background(), strataotel.Config{
		Exporter:    exp,
		SampleRatio: 1.0,
		ServiceName: "strata-test",
	})
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })

	parentTracer := p.Tracer("strata.gateway")
	ctx, root := parentTracer.Start(context.Background(), "PUT /bkt/key",
		trace.WithSpanKind(trace.SpanKindServer),
	)

	cassObs := cassandra.NewQueryObserver(nil, 0, nil, p.Tracer("strata.meta.cassandra"))
	if cassObs == nil {
		t.Fatal("cassandra observer must be non-nil with tracer set")
	}
	start := time.Now()
	cassObs.ObserveQuery(ctx, gocql.ObservedQuery{
		Statement: "INSERT INTO objects (bucket_id, key) VALUES (?, ?)",
		Start:     start,
		End:       start.Add(7 * time.Millisecond),
	})

	rados.ObserveOp(ctx, nil, nil, p.Tracer("strata.data.rados"),
		"rgw.data", "put", "obj.00000", time.Now().Add(-4*time.Millisecond), nil)

	root.End()

	if err := p.ForceFlush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}
	spans := exp.GetSpans()
	if len(spans) < 3 {
		t.Fatalf("want >=3 spans (root + cassandra + rados), got %d: %+v", len(spans), spanNames(spans))
	}

	rootID := root.SpanContext().SpanID()
	var (
		cassCount, radosCount, rootCount int
	)
	for _, s := range spans {
		switch {
		case s.Name == "PUT /bkt/key":
			rootCount++
		case startsWith(s.Name, "meta.cassandra."):
			cassCount++
			if s.Parent.SpanID() != rootID {
				t.Errorf("cassandra span %q parent=%s want %s", s.Name, s.Parent.SpanID(), rootID)
			}
		case startsWith(s.Name, "data.rados."):
			radosCount++
			if s.Parent.SpanID() != rootID {
				t.Errorf("rados span %q parent=%s want %s", s.Name, s.Parent.SpanID(), rootID)
			}
		}
	}
	if rootCount != 1 {
		t.Errorf("root spans=%d want 1; names=%v", rootCount, spanNames(spans))
	}
	if cassCount < 1 {
		t.Errorf("cassandra spans=%d want >=1; names=%v", cassCount, spanNames(spans))
	}
	if radosCount < 1 {
		t.Errorf("rados spans=%d want >=1; names=%v", radosCount, spanNames(spans))
	}
}

func startsWith(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

func spanNames(spans tracetest.SpanStubs) []string {
	out := make([]string, len(spans))
	for i, s := range spans {
		out[i] = s.Name
	}
	return out
}
