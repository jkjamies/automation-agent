// Package obs is the observability tooling: it builds and globally registers an
// OpenTelemetry tracer provider so the agent framework's native span tree (the
// invoke_agent -> call_llm -> execute_tool tree it already emits under the GenAI
// semantic conventions) is exported instead of discarded. We own the provider; the
// agent framework inherits it via the OTel global — so this package never calls the
// framework's own telemetry setup. Everything is off by default: with the exporter set
// to "none" (the default) Init is a no-op and nothing about the running service changes.
//
// Deterministic tooling — it imports no agent packages, and only internal/config reads
// the OTEL_* environment (this package takes a typed Config). See
// .agents/standards/observability.md.
package obs

import (
	"context"
	"fmt"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// Exporter selects the trace sink. The application speaks exactly these four; it never
// names a vendor. Any OTLP-native backend is reached with ExporterOTLP plus an endpoint
// (and optional headers), so switching vendors is a config change, not a code change.
const (
	// ExporterNone is the no-op default: Init installs nothing and returns a no-op
	// Shutdown, so merging this package changes nothing until an operator opts in.
	ExporterNone = "none"
	// ExporterConsole writes spans to stdout — local dev and the playground.
	ExporterConsole = "console"
	// ExporterOTLP exports OTLP over HTTP to OTLPEndpoint (any OTLP-native backend or a
	// local OpenTelemetry Collector). OTLPHeaders carries auth (e.g. a vendor API key).
	ExporterOTLP = "otlp"
	// ExporterGCP exports to Google Cloud Trace via Application Default Credentials.
	ExporterGCP = "gcp"
)

// defaultServiceVersion labels spans when Config.ServiceVersion is unset.
const defaultServiceVersion = "dev"

// Config is the typed observability configuration. internal/config reads the OTEL_*
// environment into these fields; this package never touches the environment, so the
// "only config reads env" boundary holds.
type Config struct {
	// Exporter is one of the Exporter* constants. Empty is treated as ExporterNone.
	Exporter string
	// ServiceName is the resource service.name attribute on every span.
	ServiceName string
	// ServiceVersion is the resource service.version attribute; empty uses "dev".
	ServiceVersion string
	// OTLPEndpoint is the OTLP/HTTP target URL (ExporterOTLP only). config rejects an
	// empty endpoint for that exporter, so by the time Init runs it is set.
	OTLPEndpoint string
	// OTLPHeaders is the standard OTEL_EXPORTER_OTLP_HEADERS value: comma-separated
	// key=value pairs, used as OTLP request headers (ExporterOTLP only).
	OTLPHeaders string
	// Sampler is a standard OTEL_TRACES_SAMPLER value (e.g. parentbased_always_on).
	Sampler string
}

// Shutdown flushes and releases the tracer provider. It is always safe to call (a no-op
// when tracing is disabled) and is returned from Init for a deferred call at process exit.
type Shutdown func(context.Context) error

// noopShutdown is the disabled-tracing Shutdown: nothing to flush or release.
func noopShutdown(context.Context) error { return nil }

// Init builds the tracer provider for cfg, registers it as the OTel global, and sets the
// global W3C TraceContext propagator. The agent framework then attaches its native spans
// to our provider. With ExporterNone (the default) it installs nothing and returns a
// no-op Shutdown, leaving the process exactly as it was. The returned Shutdown should be
// deferred in the entrypoint; it force-flushes buffered spans before releasing the
// provider (the scale-to-zero span-loss guard, mirrored by Flush on the request path).
func Init(ctx context.Context, cfg Config) (Shutdown, error) {
	switch cfg.Exporter {
	case "", ExporterNone:
		return noopShutdown, nil
	case ExporterConsole, ExporterOTLP, ExporterGCP:
		// build below
	default:
		return nil, fmt.Errorf("obs: unknown OTEL_TRACES_EXPORTER %q (want none|console|otlp|gcp)", cfg.Exporter)
	}

	exporter, err := newExporter(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return install(exporter, cfg), nil
}

// install builds our SDK tracer provider over exporter, sets it as the OTel global, and
// registers the W3C propagator. It is the shared tail of Init and the test seam that
// injects a recording exporter. The provider uses a BatchSpanProcessor (async export,
// efficient for the many spans an agent run emits); Flush forces it out in-request.
func install(exporter sdktrace.SpanExporter, cfg Config) Shutdown {
	version := cfg.ServiceVersion
	if version == "" {
		version = defaultServiceVersion
	}
	res := resourceFor(cfg.ServiceName, version)
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithSampler(parseSampler(cfg.Sampler)),
		sdktrace.WithBatcher(exporter),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	return tp.Shutdown
}

// resourceFor builds the resource describing this service. It is schemaless to avoid
// pinning a semantic-convention schema version; the two attributes are the stable
// service.name / service.version keys every backend understands.
func resourceFor(serviceName, version string) *resource.Resource {
	return resource.NewSchemaless(
		attribute.String("service.name", serviceName),
		attribute.String("service.version", version),
	)
}

// parseSampler maps a standard OTEL_TRACES_SAMPLER value to a Sampler. The default
// parentbased_always_on records every locally-started trace and honors an upstream
// sampling decision — correct here because trace volume is one-per-webhook (the cost is
// spans-per-trace, not trace rate). An unrecognized value falls back to the default
// rather than failing: the sampler is advisory, not a correctness gate.
func parseSampler(name string) sdktrace.Sampler {
	switch strings.TrimSpace(name) {
	case "always_on":
		return sdktrace.AlwaysSample()
	case "always_off":
		return sdktrace.NeverSample()
	case "parentbased_always_off":
		return sdktrace.ParentBased(sdktrace.NeverSample())
	case "", "parentbased_always_on":
		fallthrough
	default:
		return sdktrace.ParentBased(sdktrace.AlwaysSample())
	}
}

// Flush forces any buffered spans out through the exporter now. The HTTP middleware calls
// it before every traced handler returns: BatchSpanProcessor exports on a background
// timer, but Cloud Run throttles CPU the instant a response is sent, so an un-flushed
// trailing batch would be lost on scale-to-zero. It resolves the active global provider
// and is a no-op when tracing is disabled (the global is the framework's no-op provider).
func Flush(ctx context.Context) error {
	if tp, ok := otel.GetTracerProvider().(interface {
		ForceFlush(context.Context) error
	}); ok {
		return tp.ForceFlush(ctx)
	}
	return nil
}
