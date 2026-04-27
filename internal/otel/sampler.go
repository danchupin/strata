package otel

import (
	"context"
	"encoding/binary"

	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// samplingProcessor wraps an inner SpanProcessor and tail-samples spans
// at OnEnd: failing spans (status=Error or http.status_code >= 500) are
// always exported; everything else is gated by ratio of the trace ID's
// lower 64 bits.
type samplingProcessor struct {
	next      sdktrace.SpanProcessor
	threshold uint64
	enabled   bool
}

func newSamplingProcessor(next sdktrace.SpanProcessor, ratio float64) *samplingProcessor {
	p := &samplingProcessor{next: next}
	switch {
	case ratio <= 0:
		p.threshold = 0
		p.enabled = false
	case ratio >= 1:
		p.threshold = ^uint64(0)
		p.enabled = true
	default:
		p.threshold = uint64(ratio * float64(^uint64(0)))
		p.enabled = true
	}
	return p
}

func (p *samplingProcessor) OnStart(ctx context.Context, s sdktrace.ReadWriteSpan) {
	p.next.OnStart(ctx, s)
}

func (p *samplingProcessor) OnEnd(s sdktrace.ReadOnlySpan) {
	if p.shouldExport(s) {
		p.next.OnEnd(s)
	}
}

func (p *samplingProcessor) Shutdown(ctx context.Context) error { return p.next.Shutdown(ctx) }

func (p *samplingProcessor) ForceFlush(ctx context.Context) error {
	return p.next.ForceFlush(ctx)
}

func (p *samplingProcessor) shouldExport(s sdktrace.ReadOnlySpan) bool {
	if s.Status().Code == codes.Error {
		return true
	}
	for _, kv := range s.Attributes() {
		if string(kv.Key) == "http.status_code" && kv.Value.AsInt64() >= 500 {
			return true
		}
	}
	if !p.enabled {
		return false
	}
	tid := s.SpanContext().TraceID()
	lo := binary.BigEndian.Uint64(tid[8:16])
	return lo < p.threshold
}
