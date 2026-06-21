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
 *
 * `summaryDaily` and `summaryWeekly` are distinct agents (different commit windows and
 * titles) so the Monday cron posts a real weekly digest, not a copy of the daily one.
 */
export interface Deps {
  summaryDaily?: BaseAgent | null; // Kind.CronDaily
  summaryWeekly?: BaseAgent | null; // Kind.CronWeekly
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

  if (d.summaryDaily) {
    registerSummary(disp, d.summaryDaily, Kind.CronDaily, 'Run the daily commit digest.');
  }
  if (d.summaryWeekly) {
    registerSummary(disp, d.summaryWeekly, Kind.CronWeekly, 'Run the weekly commit digest.');
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

/**
 * Build a runner for a summary agent and register it under `kind`, driving it with the
 * given trigger text.
 */
function registerSummary(disp: Dispatcher, agent: BaseAgent, kind: Kind, trigger: string): void {
  const runner = newRunner('automation-agent', agent);
  disp.register(kind, summaryHandler(runner, trigger));
}

/** Drive the summary workflow runner for a cron envelope, with a fresh session per fire. */
export function summaryHandler(runner: ReturnType<typeof newRunner>, trigger: string): Handler {
  return async (_e: Envelope): Promise<void> => {
    const sessionId = `summary-${process.hrtime.bigint()}`;
    await drive(runner, 'system', sessionId, trigger);
  };
}
