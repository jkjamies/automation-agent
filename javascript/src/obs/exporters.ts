/**
 * Span-exporter construction for the obs package.
 *
 * Builds the one exporter selected by config. The application names no vendor: every OTLP backend
 * is reached through {@link TracesExporter.Otlp} + endpoint, and {@link TracesExporter.Gcp} is the
 * one convenience path (Cloud Trace via Application Default Credentials).
 */

import { ConsoleSpanExporter, type SpanExporter } from '@opentelemetry/sdk-trace-base';

import { type Config, TracesExporter } from './obs';

/**
 * Build the span exporter for `cfg.exporter`. The caller ({@link init}) has already rejected
 * {@link TracesExporter.None} (no exporter) and any unknown value, so this handles only the three
 * real sinks.
 *
 * The OTLP and Cloud Trace exporters are imported dynamically, not at module load: they pull in
 * heavy transitive dependencies, and the default no-exporter path must not pay that import cost.
 *
 * @throws Error on an {@link TracesExporter.Otlp} config with no endpoint, or an unknown exporter.
 */
export async function newExporter(cfg: Config): Promise<SpanExporter> {
  switch (cfg.exporter) {
    case TracesExporter.Console:
      return new ConsoleSpanExporter();
    case TracesExporter.Otlp: {
      const endpoint = (cfg.otlpEndpoint ?? '').trim();
      if (endpoint === '') {
        // config validates this, but guard so a direct caller fails loudly rather than silently
        // exporting nowhere.
        throw new Error(`obs: exporter ${JSON.stringify(TracesExporter.Otlp)} requires an OTLP endpoint`);
      }
      const { OTLPTraceExporter } = await import('@opentelemetry/exporter-trace-otlp-http');
      const headers = parseOtlpHeaders(cfg.otlpHeaders ?? '');
      return new OTLPTraceExporter({
        url: endpoint,
        headers: Object.keys(headers).length > 0 ? headers : undefined,
      });
    }
    case TracesExporter.Gcp: {
      // No project id: the Cloud Trace exporter detects it from Application Default Credentials /
      // the metadata server, matching how the rest of the cloud path authenticates.
      const { TraceExporter } = await import('@google-cloud/opentelemetry-cloud-trace-exporter');
      return new TraceExporter();
    }
    default:
      throw new Error(`obs: unknown OTEL_TRACES_EXPORTER ${JSON.stringify(cfg.exporter)}`);
  }
}

/**
 * Parse the standard `OTEL_EXPORTER_OTLP_HEADERS` form — comma-separated key=value pairs (e.g.
 * `"api-key=secret,env=prod"`) — into a header map. Blank entries and entries without a key are
 * skipped; only the first `=` splits, so a value may contain `=`.
 */
export function parseOtlpHeaders(raw: string): Record<string, string> {
  const out: Record<string, string> = {};
  for (const pairRaw of raw.split(',')) {
    const pair = pairRaw.trim();
    if (pair === '') {
      continue;
    }
    const eq = pair.indexOf('=');
    if (eq <= 0) {
      // No `=`, or an empty key: skip rather than record a keyless header.
      continue;
    }
    const key = pair.slice(0, eq).trim();
    if (key === '') {
      continue;
    }
    out[key] = pair.slice(eq + 1).trim();
  }
  return out;
}
