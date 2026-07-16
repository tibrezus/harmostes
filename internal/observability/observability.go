// Package observability owns harmostes's OpenTelemetry lifecycle: OTLP exporter
// setup, global tracer/meter providers, W3C traceparent propagation, resource
// attributes, and a Shutdown that flushes before process exit.
//
// Phase 1 wires this into every binary; no spans are emitted here yet. Later
// phases (worker pipeline, agent, plugins) obtain tracers/meters via Tracer()/
// Meter() and the lifecycle "just works" — including the critical guarantee that
// a short-lived worker Job flushes its telemetry before os.Exit.
package observability

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// ShutdownTimeout is the max time a binary will wait to flush in-flight telemetry
// at exit. A stuck exporter cannot hang process termination longer than this.
const ShutdownTimeout = 10 * time.Second

// Config drives Init. An empty OTLPEndpoint disables telemetry (otel's default
// no-op providers) — the safe default for local dev and unit tests.
type Config struct {
	Component    string // service.name, e.g. "harmostes-worker"
	Version      string // service.version (build info; optional)
	OTLPEndpoint string // OTEL_EXPORTER_OTLP_ENDPOINT; "" disables
	Insecure     bool   // plain gRPC for cluster-internal collectors
	PodName      string // k8s.pod.name (downward API)
	PodNamespace string // k8s.namespace.name
}

// ShutdownFunc flushes and tears down the providers. It is idempotent.
type ShutdownFunc func(ctx context.Context) error

// Init configures global tracer + meter providers (OTLP/gRPC) and the W3C
// traceparent propagator used by Phase 2 to join daprd spans. If OTLPEndpoint is
// empty, telemetry is disabled and the returned Shutdown is a no-op. The returned
// Shutdown MUST be called before process exit to flush in-flight telemetry —
// see the worker's finish() for the lifecycle that guarantees this.
func Init(ctx context.Context, cfg Config) (ShutdownFunc, error) {
	// W3C TraceContext (+ Baggage) so traceparent injection in Phase 2 nests daprd
	// spans under harmostes's pipeline spans. Set unconditionally — even with
	// telemetry disabled, propagation is used at the HTTP layer.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))

	if cfg.OTLPEndpoint == "" {
		// Disabled: otel's global providers default to no-op; nothing to flush.
		return func(context.Context) error { return nil }, nil
	}

	res, err := buildResource(cfg)
	if err != nil {
		return nil, err
	}

	traceOpts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(cfg.OTLPEndpoint)}
	if cfg.Insecure {
		traceOpts = append(traceOpts, otlptracegrpc.WithInsecure())
	}
	texp, err := otlptracegrpc.New(ctx, traceOpts...)
	if err != nil {
		return nil, fmt.Errorf("otel trace exporter: %w", err)
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(texp),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)

	metricOpts := []otlpmetricgrpc.Option{otlpmetricgrpc.WithEndpoint(cfg.OTLPEndpoint)}
	if cfg.Insecure {
		metricOpts = append(metricOpts, otlpmetricgrpc.WithInsecure())
	}
	mexp, err := otlpmetricgrpc.New(ctx, metricOpts...)
	if err != nil {
		return nil, fmt.Errorf("otel metric exporter: %w", err)
	}
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(mexp)),
		sdkmetric.WithResource(res),
	)
	otel.SetMeterProvider(mp)

	var once sync.Once
	var result error
	return func(ctx context.Context) error {
		once.Do(func() {
			// ForceFlush pushes buffered spans/metrics before Shutdown drains.
			_ = tp.ForceFlush(ctx)
			_ = mp.ForceFlush(ctx)
			result = errors.Join(tp.Shutdown(ctx), mp.Shutdown(ctx))
		})
		return result
	}, nil
}

// buildResource assembles the OTel Resource describing this process: service.*
// identity + k8s placement, merged over the SDK defaults (telemetry.sdk.*).
func buildResource(cfg Config) (*resource.Resource, error) {
	attrs := []attribute.KeyValue{
		attribute.String("service.name", cfg.Component),
		attribute.String("service.namespace", "harmostes"),
	}
	if cfg.Version != "" {
		attrs = append(attrs, attribute.String("service.version", cfg.Version))
	}
	if cfg.PodName != "" {
		attrs = append(attrs, attribute.String("k8s.pod.name", cfg.PodName))
	}
	if cfg.PodNamespace != "" {
		attrs = append(attrs, attribute.String("k8s.namespace.name", cfg.PodNamespace))
	}
	r := resource.NewWithAttributes("", attrs...)
	return resource.Merge(resource.Default(), r)
}

// Tracer returns the harmostes tracer from the global provider (no-op before Init
// configures a real one).
func Tracer() trace.Tracer { return otel.Tracer("harmostes") }

// Meter returns the harmostes meter from the global provider (no-op before Init).
func Meter() metric.Meter { return otel.Meter("harmostes") }

// ShutdownWithTimeout calls shutdown bounded by timeout so a hung exporter can't
// stall process exit. Binaries call this from their finish()/at-exit path.
func ShutdownWithTimeout(ctx context.Context, shutdown ShutdownFunc, timeout time.Duration) error {
	c, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return shutdown(c)
}
