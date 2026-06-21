/**
 * Builds the root dispatcher and registers available workflows.
 *
 * Cron kinds → summary; LINT → lint-fixer; COVERAGE → coverage-fixer; CI → resume (all
 * fix engines). Each handler is optional.
 */
import type { BaseAgent } from '@google/adk';

import { drive, newRunner } from '../setup/runner';
import { type Envelope, Kind } from '../../ingest/envelope';
import { Dispatcher, type Handler, type Logger } from './root';

/**
 * Wires the dispatcher. Each handler is optional. `ciResume` handles {@link Kind.CI} for
 * every fix workflow (lint, coverage) — each engine no-ops unless its check matches.
 */
export interface Deps {
  summaryAgent?: BaseAgent | null;
  lintKickoff?: Handler | null; // Kind.Lint
  coverageKickoff?: Handler | null; // Kind.Coverage
  ciResume?: Handler | null; // Kind.CI (dispatched to all fix engines)
  log?: Logger | null;
}

/**
 * Build the dispatcher and register the available workflows.
 * Cron kinds → summary; LINT → lint-fixer; COVERAGE → coverage-fixer; CI → resume.
 */
export function buildRootDispatcher(d: Deps): Dispatcher {
  const disp = new Dispatcher(d.log);

  if (d.summaryAgent) {
    const runner = newRunner('automation-agent', d.summaryAgent);
    const handler = summaryHandler(runner);
    disp.register(Kind.CronDaily, handler);
    disp.register(Kind.CronWeekly, handler);
  }
  if (d.lintKickoff) {
    disp.register(Kind.Lint, d.lintKickoff);
  }
  if (d.coverageKickoff) {
    disp.register(Kind.Coverage, d.coverageKickoff);
  }
  if (d.ciResume) {
    disp.register(Kind.CI, d.ciResume);
  }
  return disp;
}

/** Drive the summary workflow runner for a cron envelope, with a fresh session per fire. */
export function summaryHandler(runner: ReturnType<typeof newRunner>): Handler {
  return async (_e: Envelope): Promise<void> => {
    const sessionId = `summary-${process.hrtime.bigint()}`;
    await drive(runner, 'system', sessionId, 'Run the daily commit digest.');
  };
}
