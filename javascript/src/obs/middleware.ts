/**
 * The HTTP tracing middleware for the obs package.
 *
 * A connect/express-style middleware so every inbound request (except the health probe) gets a
 * server span, and the span buffer is force-flushed before the response completes.
 *
 * The server span is the trace root on the ingress path and continues the ingress trace on the
 * Cloud Tasks dispatch path: it is started from the W3C trace context read out of the inbound
 * headers via the global propagator, so a task carrying a `traceparent` header (injected by the
 * transport on enqueue) makes the dispatch span a child of the ingress span automatically.
 *
 * The flush is load-bearing, not a tuning knob: the batch span processor exports asynchronously,
 * but Cloud Run throttles CPU the instant a response is sent, so an un-flushed trailing batch is
 * lost when the instance is reclaimed. Flushing uniformly here — including on the fast 202 ingress
 * path — costs one export per request (negligible at webhook volume) and removes the scale-to-zero
 * span-loss path entirely. When tracing is disabled the middleware is a true no-op, so the wrapped
 * app behaves identically.
 */

import { type Request, type RequestHandler, type Response } from 'express';

import {
  SpanKind,
  SpanStatusCode,
  context,
  trace,
} from '@opentelemetry/api';

import { flush, isEnabled } from './obs';
import { extract } from './propagation';

/**
 * The liveness endpoint excluded from tracing: it is polled constantly and carries no causal
 * interest, so a span per probe would be pure noise — and flushing on it would ship other
 * requests' buffered batches early on the hottest path.
 */
export const HEALTH_PATH = '/healthz';

/** The instrumentation-scope name for the server spans this middleware creates. */
const TRACER_NAME = 'automation-agent/obs';

/**
 * A middleware adding a server span per request and flushing spans before the response completes.
 * Mount it before the routes (e.g. as the first `app.use`). It is a no-op when tracing is disabled
 * and skips the constantly-polled health probe.
 */
export function httpMiddleware(): RequestHandler {
  return (req: Request, res: Response, next): void => {
    // Non-traced paths get no span and no flush: tracing off (the default), or the health probe
    // (which carries no causal interest, and a flush on it would ship other requests' batches
    // early on the hottest path).
    if (!isEnabled() || req.path === HEALTH_PATH) {
      next();
      return;
    }

    const parent = extract(headerMap(req));
    const span = trace
      .getTracer(TRACER_NAME)
      .startSpan(`${req.method} ${req.path}`, { kind: SpanKind.SERVER }, parent);
    const ctx = trace.setSpan(parent, span);

    // End the span and force-flush once the response is done, whichever event fires first. The
    // flush runs after the response is written but while the request is still being handled, so
    // buffered spans ship before the instance can be reclaimed (the scale-to-zero guard).
    let finished = false;
    const finalize = (): void => {
      if (finished) {
        return;
      }
      finished = true;
      span.setAttribute('http.response.status_code', res.statusCode);
      // A 5xx is the server-error signal in this app: the handlers catch their own failures and map
      // them to a 500 rather than throwing, so the status code — not a thrown exception — is what
      // marks a request as failed on the trace.
      if (res.statusCode >= 500) {
        span.setStatus({ code: SpanStatusCode.ERROR });
      }
      span.end();
      // Export buffered spans while CPU is still allocated. An export failure is non-fatal and must
      // not surface as an unhandled promise rejection, so swallow it (the flush is best-effort).
      flush().catch(() => {});
    };
    res.on('finish', finalize);
    res.on('close', finalize);

    // Run the rest of the chain with the server span active so the framework's spans parent to it.
    context.with(ctx, () => {
      try {
        next();
      } catch (err) {
        // A synchronous throw from the handler chain: record it on the span, then re-raise so the
        // framework's own error handling is unchanged. Do not finalize here — the eventual
        // finish/close event completes the span with the real response status the framework sets.
        span.setStatus({ code: SpanStatusCode.ERROR, message: errMessage(err) });
        span.recordException(err instanceof Error ? err : new Error(errMessage(err)));
        throw err;
      }
    });
  };
}

/** Collapse express's `string | string[] | undefined` headers to a `traceparent`-friendly map,
 * taking the first value of any repeated header. */
function headerMap(req: Request): Record<string, string> {
  const out: Record<string, string> = {};
  for (const [key, value] of Object.entries(req.headers)) {
    if (Array.isArray(value)) {
      if (value[0] !== undefined) {
        out[key] = value[0];
      }
    } else if (value !== undefined) {
      out[key] = value;
    }
  }
  return out;
}

/** Extract a message from a thrown value. */
function errMessage(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}
