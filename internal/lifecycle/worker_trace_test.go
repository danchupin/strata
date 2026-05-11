package lifecycle

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/meta/memory"
	strataotel "github.com/danchupin/strata/internal/otel"
)

// flakyTransitionBackend errors the Nth PutChunks call so a transition spans
// completes with Error status — exercises the per-iteration sticky-err
// propagation path.
type flakyTransitionBackend struct {
	failOn int32
	puts   atomic.Int32
}

func (b *flakyTransitionBackend) PutChunks(_ context.Context, r io.Reader, class string) (*data.Manifest, error) {
	body, _ := io.ReadAll(r)
	if b.puts.Add(1) == b.failOn {
		return nil, errors.New("boom: put-chunks failed")
	}
	return &data.Manifest{
		Class: class,
		Size:  int64(len(body)),
		ETag:  "etag",
		Chunks: []data.ChunkRef{{Cluster: "default", Pool: "cold", OID: "oid", Size: int64(len(body))}},
	}, nil
}

func (b *flakyTransitionBackend) GetChunks(context.Context, *data.Manifest, int64, int64) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader([]byte("payload"))), nil
}

func (b *flakyTransitionBackend) Delete(context.Context, *data.Manifest) error { return nil }

func (b *flakyTransitionBackend) Close() error { return nil }

func TestLifecycleWorkerEmitsIterationAndSubOpSpans(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	ctx := context.Background()
	store := memory.New()
	be := &flakyTransitionBackend{failOn: 1}

	b, err := store.CreateBucket(ctx, "lc-trace", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	obj := &meta.Object{
		BucketID:     b.ID,
		Key:          "k",
		Size:         7,
		ETag:         "etag",
		StorageClass: "STANDARD",
		Mtime:        time.Now().Add(-2 * time.Hour),
		Manifest: &data.Manifest{Class: "STANDARD", Size: 7, ETag: "etag", Chunks: []data.ChunkRef{{
			Cluster: "default", Pool: "hot", OID: "src", Size: 7,
		}}},
	}
	if err := store.PutObject(ctx, obj, false); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	blob := []byte(`<LifecycleConfiguration><Rule><ID>r</ID><Status>Enabled</Status>
		<Filter><Prefix></Prefix></Filter>
		<Transition><Days>1</Days><StorageClass>COLD</StorageClass></Transition>
	</Rule></LifecycleConfiguration>`)
	if err := store.SetBucketLifecycle(ctx, b.ID, blob); err != nil {
		t.Fatalf("SetBucketLifecycle: %v", err)
	}

	w := &Worker{
		Meta:    store,
		Data:    be,
		Region:  "default",
		AgeUnit: time.Hour,
		Logger:  slog.Default(),
		Tracer:  tp.Tracer("strata.worker.lifecycle"),
	}
	_ = w.RunOnce(ctx)

	spans := exp.GetSpans()
	if len(spans) == 0 {
		t.Fatalf("no spans emitted")
	}

	var (
		iter       *tracetest.SpanStub
		scans      int
		transForm  int
		transError bool
	)
	for i := range spans {
		s := &spans[i]
		switch s.Name {
		case "worker.lifecycle.tick":
			iter = s
		case "lifecycle.scan_bucket":
			scans++
		case "lifecycle.transition_object":
			transForm++
			if s.Status.Code.String() == "Error" {
				transError = true
			}
		}
	}
	if iter == nil {
		t.Fatalf("missing worker.lifecycle.tick iteration span; got %v", spanNames(spans))
	}
	if iter.Status.Code.String() != "Error" {
		t.Errorf("iteration status = %s, want Error (sticky-err propagation)", iter.Status.Code)
	}
	if !hasAttr(iter.Attributes, strataotel.AttrComponentKey, "worker") {
		t.Errorf("iteration missing strata.component=worker; got %v", iter.Attributes)
	}
	if !hasAttr(iter.Attributes, strataotel.WorkerKey, "lifecycle") {
		t.Errorf("iteration missing strata.worker=lifecycle; got %v", iter.Attributes)
	}
	if scans < 1 {
		t.Errorf("lifecycle.scan_bucket spans = %d, want >=1", scans)
	}
	if transForm != 1 {
		t.Errorf("lifecycle.transition_object spans = %d, want 1", transForm)
	}
	if !transError {
		t.Errorf("expected lifecycle.transition_object span with Error status")
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
