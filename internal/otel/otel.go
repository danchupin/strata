// Package otel wires OpenTelemetry tracing into the gateway. Init reads
// OTEL_EXPORTER_OTLP_ENDPOINT (W3C-spec env var) and STRATA_OTEL_SAMPLE_RATIO,
// builds a TracerProvider with an OTLP/HTTP exporter, and installs the
// W3C TraceContext propagator. When the endpoint env var is unset Init
// returns a no-op provider so callers stay free of nil checks.
//
// Sampling is tail-based: the trace is always recorded, then a wrapping
// SpanProcessor decides on OnEnd whether to forward to the exporter.
// Spans whose status is Error or carry http.status_code >= 500 are always
// exported regardless of ratio.
package otel

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/danchupin/strata/internal/otel/ringbuf"
)

const (
	// EnvEndpoint is the standard OTLP endpoint env var.
	EnvEndpoint = "OTEL_EXPORTER_OTLP_ENDPOINT"
	// EnvSampleRatio is the strata-specific tail-sampling ratio (0..1).
	EnvSampleRatio = "STRATA_OTEL_SAMPLE_RATIO"
	// EnvRingbuf toggles the in-process trace ring buffer (US-005). Default
	// "on"; set to "off"/"false"/"0" to disable.
	EnvRingbuf = "STRATA_OTEL_RINGBUF"
	// EnvRingbufBytes overrides the ring buffer's bytes budget. Default
	// ringbuf.DefaultBytesBudget (4 MiB).
	EnvRingbufBytes = "STRATA_OTEL_RINGBUF_BYTES"
	// DefaultSampleRatio is applied when EnvSampleRatio is unset / invalid.
	DefaultSampleRatio = 0.01
	// DefaultServiceName is the resource service.name attribute used when
	// Config.ServiceName is empty.
	DefaultServiceName = "strata"
	// TracerName is the instrumentation library name attached to spans.
	TracerName = "github.com/danchupin/strata/internal/otel"
)

// Config tunes Init. Endpoint empty + Exporter nil + Ringbuf=false yields a
// no-op provider. Exporter overrides the default OTLP/HTTP exporter (used by
// tests). When Ringbuf is true an in-process ring buffer span processor
// (US-005) is installed alongside any configured OTLP forwarder so the
// /admin/v1/diagnostics/trace/{requestID} handler always has data even when
// no Jaeger / Tempo deployment is wired up.
type Config struct {
	Endpoint    string
	SampleRatio float64
	ServiceName string

	Exporter sdktrace.SpanExporter

	Ringbuf        bool
	RingbufBytes   int
	RingbufLogger  *slog.Logger
	RingbufMetrics ringbuf.MetricsSink
}

// InitOptions plugs binary-side dependencies into Init without forcing the
// caller to read the env. Logger seeds the ring buffer's WARN sink;
// RingbufMetrics is the Prometheus adapter.
type InitOptions struct {
	Logger         *slog.Logger
	RingbufMetrics ringbuf.MetricsSink
}

// Provider holds the SDK tracer provider plus a Shutdown shim. Methods
// are nil-safe — a no-op provider returns the global tracer and a no-op
// Shutdown.
type Provider struct {
	tp       *sdktrace.TracerProvider
	shutdown func(context.Context) error
	rb       *ringbuf.RingBuffer
}

// Ringbuf returns the in-process trace ring buffer if installed, else nil.
// adminapi consumes this for the /admin/v1/diagnostics/trace/{requestID}
// endpoint.
func (p *Provider) Ringbuf() *ringbuf.RingBuffer {
	if p == nil {
		return nil
	}
	return p.rb
}

// Tracer returns a tracer scoped to name. Falls back to the global
// tracer provider when the underlying SDK provider is absent (no-op mode).
func (p *Provider) Tracer(name string) trace.Tracer {
	if p == nil || p.tp == nil {
		return otel.Tracer(name)
	}
	return p.tp.Tracer(name)
}

// ForceFlush drains the BatchSpanProcessor — used by tests. No-op when
// the underlying provider is nil.
func (p *Provider) ForceFlush(ctx context.Context) error {
	if p == nil || p.tp == nil {
		return nil
	}
	return p.tp.ForceFlush(ctx)
}

// Shutdown flushes and stops the exporter. Safe to call on a no-op
// provider.
func (p *Provider) Shutdown(ctx context.Context) error {
	if p == nil || p.shutdown == nil {
		return nil
	}
	return p.shutdown(ctx)
}

// Init builds a Provider from the environment. Reads OTEL_EXPORTER_OTLP_ENDPOINT,
// STRATA_OTEL_SAMPLE_RATIO, STRATA_OTEL_RINGBUF, STRATA_OTEL_RINGBUF_BYTES.
// Pass logger + metrics through opts so the ring buffer's WARN/gauge wiring
// stays out of the env layer.
func Init(ctx context.Context, opts InitOptions) (*Provider, error) {
	return InitWithConfig(ctx, Config{
		Endpoint:       os.Getenv(EnvEndpoint),
		SampleRatio:    ratioFromEnv(),
		ServiceName:    DefaultServiceName,
		Ringbuf:        ringbufFromEnv(),
		RingbufBytes:   ringbufBytesFromEnv(),
		RingbufLogger:  opts.Logger,
		RingbufMetrics: opts.RingbufMetrics,
	})
}

// InitWithConfig is the lower-level variant tests use to inject an
// in-memory exporter.
func InitWithConfig(ctx context.Context, cfg Config) (*Provider, error) {
	if cfg.ServiceName == "" {
		cfg.ServiceName = DefaultServiceName
	}

	wantOTLP := cfg.Exporter != nil || cfg.Endpoint != ""

	if !wantOTLP && !cfg.Ringbuf {
		otel.SetTracerProvider(tracenoop.NewTracerProvider())
		otel.SetTextMapPropagator(propagation.TraceContext{})
		return &Provider{}, nil
	}

	var processors []sdktrace.SpanProcessor
	if wantOTLP {
		exporter := cfg.Exporter
		if exporter == nil {
			opts, err := otlpHTTPOptions(cfg.Endpoint)
			if err != nil {
				return nil, err
			}
			exp, err := otlptrace.New(ctx, otlptracehttp.NewClient(opts...))
			if err != nil {
				return nil, fmt.Errorf("otlp exporter: %w", err)
			}
			exporter = exp
		}
		bsp := sdktrace.NewBatchSpanProcessor(exporter)
		processors = append(processors, newSamplingProcessor(bsp, cfg.SampleRatio))
	}

	var rb *ringbuf.RingBuffer
	if cfg.Ringbuf {
		rb = ringbuf.New(
			ringbuf.WithBytes(cfg.RingbufBytes),
			ringbuf.WithMetrics(cfg.RingbufMetrics),
			ringbuf.WithLogger(cfg.RingbufLogger),
		)
		processors = append(processors, rb)
	}

	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(cfg.ServiceName),
	))
	if err != nil {
		return nil, fmt.Errorf("otel resource: %w", err)
	}

	tpOpts := []sdktrace.TracerProviderOption{
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithResource(res),
	}
	for _, p := range processors {
		tpOpts = append(tpOpts, sdktrace.WithSpanProcessor(p))
	}
	tp := sdktrace.NewTracerProvider(tpOpts...)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	return &Provider{tp: tp, shutdown: tp.Shutdown, rb: rb}, nil
}

func ringbufFromEnv() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(EnvRingbuf)))
	if v == "" {
		return true
	}
	switch v {
	case "off", "false", "0", "no":
		return false
	default:
		return true
	}
}

func ringbufBytesFromEnv() int {
	v := os.Getenv(EnvRingbufBytes)
	if v == "" {
		return ringbuf.DefaultBytesBudget
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return ringbuf.DefaultBytesBudget
	}
	return n
}

func ratioFromEnv() float64 {
	v := os.Getenv(EnvSampleRatio)
	if v == "" {
		return DefaultSampleRatio
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0 {
		return DefaultSampleRatio
	}
	if f > 1 {
		return 1
	}
	return f
}

func otlpHTTPOptions(endpoint string) ([]otlptracehttp.Option, error) {
	return []otlptracehttp.Option{otlptracehttp.WithEndpointURL(endpoint)}, nil
}
