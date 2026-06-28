/**
 * The execution transport between webhook ingress and the dispatcher.
 *
 * Webhook ingress reduces a request to an {@link Envelope} and calls
 * {@link Transport.enqueue}, which returns fast; the envelope's workflow runs *later* — in a
 * background task for the in-process backend, or in a fresh `/internal/dispatch` request
 * delivered by Cloud Tasks in production. The seam exists because on Cloud Run with
 * request-based billing CPU is throttled to near-zero once a response is sent, so
 * multi-minute LLM compute must run *inside* a request (Cloud Tasks gives that, plus durable
 * retry and rate limiting). See `specs/20260626-workflow-execution-transport.md`.
 * Deterministic tooling — no agent imports (the dispatcher is injected as a
 * {@link DispatchFunc}).
 */

import { type Envelope } from '../ingest/envelope';

/**
 * Runs the work for one envelope. It is the root dispatcher's `dispatch`, passed in so this
 * package stays decoupled from the agent layer.
 */
export type DispatchFunc = (e: Envelope) => Promise<void>;

/** Structured logger for non-fatal transport diagnostics. */
export interface Logger {
  info(msg: string, fields?: Record<string, unknown>): void;
  warn(msg: string, fields?: Record<string, unknown>): void;
  error(msg: string, fields?: Record<string, unknown>): void;
}

/**
 * Optional, backend-honored hints. The transport stays deliberately dumb about workflow
 * semantics: it carries these to the backend without interpreting them — coalesce-to-latest /
 * staleness logic lives in the workflow, not here (spec Decision §3). Both are
 * Cloud-Tasks-only; the in-process backend ignores them (an immediate, undeduplicated
 * dispatch).
 */
export interface EnqueueOptions {
  /**
   * A Cloud Tasks dedup key: a duplicate task with the same name is dropped for ~1h, giving
   * idempotency against a redelivered webhook. Empty means no dedup.
   */
  name?: string;
  /**
   * Schedule delivery this many milliseconds in the future (e.g. a review debounce window).
   * Zero means deliver immediately.
   */
  delayMs?: number;
}

/**
 * Enqueues an envelope for asynchronous execution and returns quickly. A rejected promise
 * becomes a 500 to the webhook caller (so GitHub / Cloud Scheduler retries).
 */
export interface Transport {
  /** Schedule `e` for execution. `opts` carry optional, backend-honored hints. */
  enqueue(e: Envelope, opts?: EnqueueOptions): Promise<void>;
  /**
   * Release the backend: the in-process backend drains in-flight tasks; the Cloud Tasks
   * backend closes its gRPC client. Safe to call once at shutdown.
   */
  close(): Promise<void>;
}
