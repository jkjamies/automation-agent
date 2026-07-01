/*
 * The tracer-provider registration at the heart of the obs package.
 *
 * It builds and globally registers an OpenTelemetry tracer provider so the agent framework's native
 * span tree (the invoke_agent -> call_llm -> execute_tool tree it already emits under the GenAI
 * semantic conventions) is exported instead of discarded. We own the provider; the agent framework
 * inherits it via the OTel global — its default tracer resolves the global provider on first use, so
 * registering ours first is all it takes, and this package never calls the framework's own telemetry
 * setup. Everything is off by default: with the exporter set to "none" (the default) [init] is a
 * no-op and nothing about the running service changes.
 *
 * Deterministic tooling — it imports no agent packages, and only config reads the OTEL_*
 * environment (this package takes a typed [Config]). See .agents/standards/observability.md.
 */
package com.automation.agent.obs

import io.opentelemetry.api.GlobalOpenTelemetry
import io.opentelemetry.api.common.AttributeKey
import io.opentelemetry.api.common.Attributes
import io.opentelemetry.api.trace.Tracer
import io.opentelemetry.api.trace.propagation.W3CTraceContextPropagator
import io.opentelemetry.context.propagation.ContextPropagators
import io.opentelemetry.sdk.OpenTelemetrySdk
import io.opentelemetry.sdk.resources.Resource
import io.opentelemetry.sdk.trace.SdkTracerProvider
import io.opentelemetry.sdk.trace.export.BatchSpanProcessor
import io.opentelemetry.sdk.trace.export.SpanExporter
import io.opentelemetry.sdk.trace.samplers.Sampler
import java.util.concurrent.TimeUnit

/**
 * The trace sink values. The application speaks exactly these four; it never names a vendor. Any
 * OTLP-native backend is reached with [EXPORTER_OTLP] plus an endpoint (and optional headers), so
 * switching vendors is a config change, not a code change.
 */
/** The no-op default: init installs nothing, so merging changes nothing until an operator opts in. */
const val EXPORTER_NONE = "none"

/** Spans as text on stdout — local dev and the playground. */
const val EXPORTER_CONSOLE = "console"

/** OTLP over HTTP to an endpoint (any OTLP-native backend or a local Collector). */
const val EXPORTER_OTLP = "otlp"

/** Google Cloud Trace via Application Default Credentials. */
const val EXPORTER_GCP = "gcp"

/** Labels spans when [Config.serviceVersion] is unset. */
const val DEFAULT_SERVICE_VERSION = "dev"

/**
 * Bounds a force-flush. It is only a backstop for a pathologically wedged exporter, so it sits above
 * the exporters' own request timeouts (OTLP / Cloud Trace default to ~10s): a tighter bound could
 * cancel a slow-but-working export and lose the very trailing batch the flush exists to guarantee.
 */
const val FLUSH_TIMEOUT_MS = 30_000L

/** The instrumentation-scope name for the server spans the middleware creates. */
internal const val TRACER_NAME = "automation-agent/obs"

/**
 * The typed observability configuration. config reads the OTEL_* environment into these fields; this
 * package never touches the environment, so the "only config reads env" boundary holds.
 */
data class Config(
    /** One of the exporter values. Empty is treated as [EXPORTER_NONE]. */
    val exporter: String = EXPORTER_NONE,
    /** The resource service.name attribute on every span. */
    val serviceName: String = "automation-agent",
    /** The resource service.version attribute; empty uses [DEFAULT_SERVICE_VERSION]. */
    val serviceVersion: String = "",
    /**
     * The OTLP/HTTP target URL ([EXPORTER_OTLP] only). config rejects an empty endpoint for that
     * exporter, so by the time init runs it is set.
     */
    val otlpEndpoint: String = "",
    /**
     * The standard OTEL_EXPORTER_OTLP_HEADERS value: comma-separated key=value pairs, used as OTLP
     * request headers ([EXPORTER_OTLP] only).
     */
    val otlpHeaders: String = "",
    /** A standard OTEL_TRACES_SAMPLER value (e.g. parentbased_always_on). */
    val sampler: String = "parentbased_always_on",
)

/**
 * Flushes and releases the tracer provider. Always safe to call (a no-op when tracing is disabled)
 * and returned from [init] for the entrypoint's shutdown path.
 */
typealias Shutdown = () -> Unit

private val noopShutdown: Shutdown = {}

// The provider we registered, kept so flush/shutdown can reach it directly. Null means tracing is
// disabled — every entry point is a no-op.
@Volatile
private var activeProvider: SdkTracerProvider? = null

/**
 * Whether tracing is enabled (a provider was registered). The middleware uses it to stay a true
 * no-op on the default path.
 */
fun isEnabled(): Boolean = activeProvider != null

/**
 * Build the tracer provider for [cfg], register it as the OTel global, and set the global W3C
 * TraceContext propagator. The agent framework then attaches its native spans to our provider. With
 * [EXPORTER_NONE] (the default) it installs nothing and returns a no-op [Shutdown], leaving the
 * process exactly as it was. The returned Shutdown should be called in the entrypoint's shutdown
 * path; it force-flushes buffered spans before releasing the provider (the scale-to-zero span-loss
 * guard, mirrored by [flush] on the request path).
 *
 * @throws IllegalArgumentException if [cfg] exporter is not one of none|console|otlp|gcp.
 */
fun init(cfg: Config): Shutdown {
    val name = cfg.exporter.ifBlank { EXPORTER_NONE }
    if (name == EXPORTER_NONE) return noopShutdown
    if (name != EXPORTER_CONSOLE && name != EXPORTER_OTLP && name != EXPORTER_GCP) {
        throw IllegalArgumentException("obs: unknown OTEL_TRACES_EXPORTER \"$name\" (want none|console|otlp|gcp)")
    }
    return install(newExporter(cfg), cfg)
}

/**
 * Build the SDK tracer provider over [exporter], register the assembled SDK as the OTel global, and
 * set the W3C propagator. This is the shared tail of [init] and the test seam that injects a
 * recording exporter. The provider uses a batch span processor (async export, efficient for the many
 * spans an agent run emits); [flush] forces it out in-request.
 */
internal fun install(exporter: SpanExporter, cfg: Config): Shutdown {
    val version = cfg.serviceVersion.ifBlank { DEFAULT_SERVICE_VERSION }
    val provider = SdkTracerProvider.builder()
        .setResource(resourceFor(cfg.serviceName, version))
        .setSampler(parseSampler(cfg.sampler))
        .addSpanProcessor(BatchSpanProcessor.builder(exporter).build())
        .build()
    val sdk = OpenTelemetrySdk.builder()
        .setTracerProvider(provider)
        .setPropagators(ContextPropagators.create(W3CTraceContextPropagator.getInstance()))
        .build()
    // GlobalOpenTelemetry.set refuses a second registration (throws), so a double init is a loud
    // wiring bug rather than a silently-ignored provider whose spans the framework never uses. init
    // runs once per process. Tests reset the global with GlobalOpenTelemetry.resetForTest.
    GlobalOpenTelemetry.set(sdk)
    activeProvider = provider

    return {
        // shutdown() force-flushes buffered spans, then releases the processor.
        provider.shutdown().join(FLUSH_TIMEOUT_MS, TimeUnit.MILLISECONDS)
        activeProvider = null
    }
}

/**
 * Build the resource describing this service. The two attributes are the stable
 * service.name / service.version keys every backend understands.
 */
private fun resourceFor(serviceName: String, version: String): Resource =
    Resource.create(
        Attributes.of(
            AttributeKey.stringKey("service.name"), serviceName,
            AttributeKey.stringKey("service.version"), version,
        ),
    )

/**
 * Map a standard OTEL_TRACES_SAMPLER value to a Sampler. The default parentbased_always_on records
 * every locally-started trace and honors an upstream sampling decision — correct here because trace
 * volume is one-per-webhook (the cost is spans-per-trace, not trace rate). An unrecognized value
 * falls back to the default rather than failing: the sampler is advisory, not a correctness gate.
 */
fun parseSampler(name: String): Sampler = when (name.trim()) {
    "always_on" -> Sampler.alwaysOn()
    "always_off" -> Sampler.alwaysOff()
    "parentbased_always_off" -> Sampler.parentBased(Sampler.alwaysOff())
    else -> Sampler.parentBased(Sampler.alwaysOn())
}

/**
 * Force any buffered spans out through the exporter now. The HTTP middleware calls it before every
 * traced response returns: the batch span processor exports on a background timer, but Cloud Run
 * throttles CPU the instant a response is sent, so an un-flushed trailing batch would be lost on
 * scale-to-zero. It is a no-op when tracing is disabled.
 */
fun flush() {
    activeProvider?.forceFlush()?.join(FLUSH_TIMEOUT_MS, TimeUnit.MILLISECONDS)
}

/** The tracer the middleware starts server spans with; resolves the registered global provider. */
internal fun tracer(): Tracer = GlobalOpenTelemetry.getTracer(TRACER_NAME)
