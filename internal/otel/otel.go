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
	"os"
	"strconv"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

const (
	// EnvEndpoint is the standard OTLP endpoint env var.
	EnvEndpoint = "OTEL_EXPORTER_OTLP_ENDPOINT"
	// EnvSampleRatio is the strata-specific tail-sampling ratio (0..1).
	EnvSampleRatio = "STRATA_OTEL_SAMPLE_RATIO"
	// DefaultSampleRatio is applied when EnvSampleRatio is unset / invalid.
	DefaultSampleRatio = 0.01
	// DefaultServiceName is the resource service.name attribute used when
	// Config.ServiceName is empty.
	DefaultServiceName = "strata"
	// TracerName is the instrumentation library name attached to spans.
	TracerName = "github.com/danchupin/strata/internal/otel"
)

// Config tunes Init. Endpoint empty => no-op provider. Exporter overrides
// the default OTLP/HTTP exporter (used by tests).
type Config struct {
	Endpoint    string
	SampleRatio float64
	ServiceName string

	Exporter sdktrace.SpanExporter
}

// Provider holds the SDK tracer provider plus a Shutdown shim. Methods
// are nil-safe — a no-op provider returns the global tracer and a no-op
// Shutdown.
type Provider struct {
	tp       *sdktrace.TracerProvider
	shutdown func(context.Context) error
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

// Init builds a Provider from the environment.
func Init(ctx context.Context) (*Provider, error) {
	return InitWithConfig(ctx, Config{
		Endpoint:    os.Getenv(EnvEndpoint),
		SampleRatio: ratioFromEnv(),
		ServiceName: DefaultServiceName,
	})
}

// InitWithConfig is the lower-level variant tests use to inject an
// in-memory exporter.
func InitWithConfig(ctx context.Context, cfg Config) (*Provider, error) {
	if cfg.ServiceName == "" {
		cfg.ServiceName = DefaultServiceName
	}
	if cfg.Exporter == nil && cfg.Endpoint == "" {
		otel.SetTracerProvider(tracenoop.NewTracerProvider())
		otel.SetTextMapPropagator(propagation.TraceContext{})
		return &Provider{}, nil
	}

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

	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(cfg.ServiceName),
	))
	if err != nil {
		return nil, fmt.Errorf("otel resource: %w", err)
	}

	bsp := sdktrace.NewBatchSpanProcessor(exporter)
	sampling := newSamplingProcessor(bsp, cfg.SampleRatio)

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSpanProcessor(sampling),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	return &Provider{tp: tp, shutdown: tp.Shutdown}, nil
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
