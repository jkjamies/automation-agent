/**
 * obs — observability tooling: turn on the distributed tracing the agent framework already emits
 * but discards.
 *
 * The framework builds a native span tree (`invoke_agent` -> `call_llm` -> `execute_tool`, under
 * the GenAI semantic conventions) for every run, but the spans go nowhere until a process
 * registers a tracer provider + exporter once at startup. This package is that registration, plus
 * the propagation and flush plumbing that stitches the trace across the Cloud Tasks boundary. Off
 * by default (`OTEL_TRACES_EXPORTER=none`). Deterministic tooling — no agent imports; only `config`
 * reads `OTEL_*`. See `.agents/standards/observability.md`.
 */

export { parseOtlpHeaders, newExporter } from './exporters';
export { newLogHandler, type Logger } from './log';
export { HEALTH_PATH, httpMiddleware } from './middleware';
export {
  DEFAULT_SERVICE_VERSION,
  TracesExporter,
  type Config,
  type Shutdown,
  flush,
  init,
  install,
  isEnabled,
  parseSampler,
} from './obs';
export { extract, inject } from './propagation';
