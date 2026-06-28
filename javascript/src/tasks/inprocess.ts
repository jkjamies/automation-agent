/** The in-process execution transport — the local-dev and default backend. */

import { type Envelope } from '../ingest/envelope';
import { type EnqueueOptions, type DispatchFunc, type Logger, type Transport } from './transport';

/** DEFAULT_MAX_CONCURRENT bounds in-flight in-process dispatches under burst (backpressure). */
export const DEFAULT_MAX_CONCURRENT = 32;

/** DRAIN_TIMEOUT_MS caps how long close() waits for in-flight dispatches to finish. */
export const DRAIN_TIMEOUT_MS = 15_000;

/** A logger that drops everything — the fallback when none is injected (nil-logger guard). */
const NOOP_LOGGER: Logger = { info() {}, warn() {}, error() {} };

/**
 * Runs each envelope in a detached promise on a bounded pool — the local-dev and default
 * backend.
 *
 * It reproduces the pre-transport behavior exactly: a burst applies backpressure (a bounded
 * permit pool), and a clean SIGTERM drains in-flight work via {@link close}. It does NOT
 * survive an instance being reclaimed mid-run, which is precisely why production uses the
 * Cloud Tasks backend instead. The `name` / `delayMs` hints are Cloud Tasks features and are
 * ignored here (an immediate, undeduplicated dispatch).
 */
export class InProcess implements Transport {
  private readonly dispatchFn: DispatchFunc;
  private readonly log: Logger;
  private readonly maxConcurrent: number;
  // Available concurrency permits; waiters are resolved FIFO as permits free (a counting
  // semaphore). A burst blocks the handler (backpressure) instead of piling up detached
  // promises.
  private permits: number;
  private readonly waiters: Array<() => void> = [];
  // In-flight dispatch promises, kept so close() can drain outstanding work rather than drop it.
  private readonly pending = new Set<Promise<void>>();
  // Set by close() to stop accepting new work before the drain, so enqueue cannot spawn a
  // dispatch the drain would miss.
  private closed = false;

  constructor(dispatch: DispatchFunc, log?: Logger | null, maxConcurrent = DEFAULT_MAX_CONCURRENT) {
    this.dispatchFn = dispatch;
    this.log = log ?? NOOP_LOGGER;
    this.maxConcurrent = maxConcurrent < 1 ? DEFAULT_MAX_CONCURRENT : maxConcurrent;
    this.permits = this.maxConcurrent;
  }

  /**
   * Dispatch `e` on the bounded pool. Blocks while the pool is full (backpressure under
   * burst) and otherwise returns immediately; the dispatch error is logged, not raised,
   * because the webhook response has already gone out. `name` / `delayMs` are ignored (Cloud
   * Tasks features).
   *
   * @throws Error if the transport has been closed (shutdown has begun).
   */
  async enqueue(e: Envelope, _opts?: EnqueueOptions): Promise<void> {
    if (this.closed) {
      // Shutdown has begun: refuse new work rather than spawn a dispatch the drain has already
      // stopped waiting for (it would be abandoned on exit).
      throw new Error('tasks: in-process transport is closed');
    }
    // When every permit is held, acquire() blocks here — the intended backpressure. Surface it
    // so sustained saturation is observable rather than silent (delayed webhook ACKs).
    if (this.permits === 0) {
      this.log.warn(
        'dispatch concurrency saturated; webhook ingest is applying backpressure until a slot frees',
        { maxConcurrent: this.maxConcurrent },
      );
    }
    await this.acquire();
    // Recheck after the (possibly long) backpressure wait: close() may have begun while we were
    // blocked on a permit. Without this, a dispatch could slip past the drain's snapshot and be
    // abandoned on exit (a recheck-after-acquire guard).
    if (this.closed) {
      this.release();
      throw new Error('tasks: in-process transport is closed');
    }
    const task = this.dispatchAndRelease(e);
    this.pending.add(task);
    void task.finally(() => this.pending.delete(task));
  }

  /**
   * Run one dispatch detached from the originating webhook request (already returned), so
   * cancelling that request does not affect it, releasing its permit when done. The error is
   * logged, not raised, because the response has already gone out.
   */
  private async dispatchAndRelease(e: Envelope): Promise<void> {
    try {
      await this.dispatchFn(e);
    } catch (err) {
      this.log.error('dispatch failed', {
        kind: e.kind,
        source: e.source,
        err: (err as Error).message,
      });
    } finally {
      this.release();
    }
  }

  /**
   * Drain in-flight dispatches (bounded by {@link DRAIN_TIMEOUT_MS}) so a clean SIGTERM
   * finishes work in flight rather than abandoning it.
   */
  async close(): Promise<void> {
    // Stop accepting new work before waiting, so enqueue cannot spawn a dispatch the drain
    // would miss.
    this.closed = true;
    if (this.pending.size === 0) {
      return;
    }
    this.log.info('draining in-flight dispatch(es)', { count: this.pending.size });
    // Wait on a snapshot, racing a timeout. On timeout we only stop waiting and do NOT abort
    // the still-running dispatches (a running promise cannot be cancelled anyway); they run to
    // completion past the drain deadline, and process exit is what ultimately ends them.
    const drained = Promise.allSettled([...this.pending]).then(() => 'drained' as const);
    let timer: ReturnType<typeof setTimeout> | undefined;
    const timedOut = new Promise<'timeout'>((resolve) => {
      timer = setTimeout(() => resolve('timeout'), DRAIN_TIMEOUT_MS);
      timer.unref?.();
    });
    try {
      if ((await Promise.race([drained, timedOut])) === 'timeout') {
        this.log.warn('drain timed out; dispatch(es) abandoned', { count: this.pending.size });
        return;
      }
      this.log.info('drained in-flight work');
    } finally {
      clearTimeout(timer);
    }
  }

  /** Take a permit, parking (FIFO) when the pool is full. */
  private acquire(): Promise<void> {
    if (this.permits > 0) {
      this.permits -= 1;
      return Promise.resolve();
    }
    return new Promise<void>((resolve) => this.waiters.push(resolve));
  }

  /** Return a permit, handing it directly to the next waiter when one is parked. */
  private release(): void {
    const next = this.waiters.shift();
    if (next) {
      next(); // hand the permit straight to the next waiter
    } else {
      this.permits += 1;
    }
  }
}
