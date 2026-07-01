/**
 * Log <-> trace correlation for the obs package.
 *
 * Wraps the injected structured logger so every call made while a span is active also carries that
 * span's `trace_id` and `span_id`. This lets a backend pivot from a log line to the trace it
 * belongs to (and, on the cloud path, the trace console auto-links the two). It reads the active
 * span from the current context, so it is zero-cost when no span is active or tracing is off — the
 * fields are then passed through untouched.
 */

import { isSpanContextValid, trace } from '@opentelemetry/api';

/** The structured logger shape shared across the service (level -> `(msg, fields?) => void`). */
export interface Logger {
  info(msg: string, fields?: Record<string, unknown>): void;
  warn(msg: string, fields?: Record<string, unknown>): void;
  error(msg: string, fields?: Record<string, unknown>): void;
}

/**
 * Wrap `base` so records emitted under an active span gain `trace_id` / `span_id`. The entrypoint
 * wraps the one injected logger once and hands the result to the request-path subsystems;
 * correlation then applies to any log call made while a span is active. A call with no active span
 * (or with tracing off) delegates with its fields untouched.
 */
export function newLogHandler(base: Logger): Logger {
  const wrap =
    (fn: (msg: string, fields?: Record<string, unknown>) => void) =>
    (msg: string, fields?: Record<string, unknown>): void => {
      fn(msg, withTraceIds(fields));
    };
  return {
    info: wrap(base.info.bind(base)),
    warn: wrap(base.warn.bind(base)),
    error: wrap(base.error.bind(base)),
  };
}

/** Merge the active span's ids into `fields`, or return `fields` unchanged when none is active. */
function withTraceIds(fields?: Record<string, unknown>): Record<string, unknown> | undefined {
  const sc = trace.getActiveSpan()?.spanContext();
  if (sc && isSpanContextValid(sc)) {
    return { ...fields, trace_id: sc.traceId, span_id: sc.spanId };
  }
  return fields;
}
