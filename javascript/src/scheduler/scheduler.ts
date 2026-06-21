/**
 * Turn cron schedules into ingest envelopes.
 *
 * Each fire emits a normalized {@link Envelope} so the root agent treats
 * time-based triggers exactly like any other ingress. Deterministic tooling —
 * no agent imports. See docs/architecture.md §2.
 */

import { Cron } from 'croner';

import { type Envelope, type Kind, newEnvelope } from '../ingest/envelope';

/** EmitFunc receives an envelope when a schedule fires. */
export type EmitFunc = (envelope: Envelope) => void;

/** Registers cron specs that emit ingest envelopes. */
export class Scheduler {
  private readonly emit: EmitFunc;
  private now: () => Date;
  private readonly jobs: Cron[] = [];
  private running = false;

  /**
   * Build a Scheduler that calls `emit` on each fire.
   *
   * @param emit - invoked with an {@link Envelope} on every schedule fire.
   * @param now - injectable clock for deterministic tests (defaults to `Date.now`).
   */
  constructor(emit: EmitFunc, now: () => Date = () => new Date()) {
    this.emit = emit;
    this.now = now;
  }

  /**
   * Register a 5-field cron spec (minute hour dom month dow) that emits an
   * envelope of the given kind, e.g. `0 9 * * *` daily, `0 9 * * 1` Mondays.
   *
   * Schedules are interpreted in UTC (not the host's local zone) so "0 9 * * *" means
   * 09:00 UTC regardless of where the process runs.
   *
   * Jobs are created paused and only begin firing once {@link start} is called.
   *
   * @throws Error for an invalid spec.
   */
  add(spec: string, kind: Kind): void {
    const job = new Cron(spec, { paused: !this.running, timezone: 'UTC' }, () =>
      this.trigger(kind),
    );
    this.jobs.push(job);
  }

  /**
   * Emit one envelope; separated from the cron closure so it is directly
   * unit-testable without waiting for a real schedule.
   */
  private trigger(kind: Kind): void {
    this.emit(newEnvelope(kind, 'scheduler', Buffer.alloc(0), this.now()));
  }

  /** Begin the cron loop (non-blocking). */
  start(): void {
    this.running = true;
    for (const job of this.jobs) {
      job.resume();
    }
  }

  /** Halt scheduling; running jobs are cancelled. */
  stop(): void {
    this.running = false;
    for (const job of this.jobs) {
      job.stop();
    }
  }

  /** Report the number of registered schedules (useful for assertions). */
  entries(): number {
    return this.jobs.length;
  }
}
