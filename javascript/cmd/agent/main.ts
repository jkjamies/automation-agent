/**
 * The automation-agent service entrypoint.
 *
 * Wires configuration, tooling, agents, the scheduler, and the webhook server together,
 * then runs until interrupted. Composition only — logic lives in `src/`.
 *
 * Run with `tsx cmd/agent/main.ts` (or `make run`).
 */
import type { BaseAgent } from '@google/adk';

import { newCoverageEngine } from '../../src/agent/covfixer/index';
import { type Deps as FixDeps, type Engine } from '../../src/agent/fixflow/index';
import { newLintEngine } from '../../src/agent/lintfixer/index';
import { buildRootDispatcher } from '../../src/agent/root/agentsSetup';
import { buildLLM, buildCodeLLM } from '../../src/agent/setup/llm';
import { buildSummaryAgent } from '../../src/agent/summary/agentsSetup';
import type { CommitLister } from '../../src/agent/summary/summary';
import { type Config, load } from '../../src/config/config';
import { Client } from '../../src/githubapi/client';
import { type Envelope, Kind } from '../../src/ingest/envelope';
import { type Notifier, newNotifier } from '../../src/notify/notify';
import { Scheduler } from '../../src/scheduler/scheduler';
import { Server } from '../../src/webhook/server';

const log = {
  info: (msg: string, fields?: Record<string, unknown>) => emit('INFO', msg, fields),
  warn: (msg: string, fields?: Record<string, unknown>) => emit('WARN', msg, fields),
  error: (msg: string, fields?: Record<string, unknown>) => emit('ERROR', msg, fields),
};

function emit(level: string, msg: string, fields?: Record<string, unknown>): void {
  const extra = fields && Object.keys(fields).length > 0 ? ' ' + JSON.stringify(fields) : '';
  console.log(`${new Date().toISOString()} ${level} automation-agent ${msg}${extra}`);
}

function buildNotifier(cfg: Config): Notifier | null {
  try {
    return newNotifier(cfg.notifyProvider, cfg.slackWebhookUrl, cfg.teamsWebhookUrl);
  } catch (err) {
    log.warn(`notifier not configured; summary disabled and fixers won't post: ${(err as Error).message}`);
    return null;
  }
}

function buildSummary(
  cfg: Config,
  llm: ReturnType<typeof buildLLM>,
  gh: CommitLister,
  notifier: Notifier | null,
): BaseAgent | null {
  if (cfg.repos.length === 0) {
    log.warn('no REPOS configured; summary workflow disabled');
    return null;
  }
  if (!notifier) {
    return null; // buildNotifier already warned
  }
  try {
    return buildSummaryAgent({ llm, gh, notify: notifier, repos: cfg.repos });
  } catch (err) {
    log.warn(`summary workflow disabled: ${(err as Error).message}`);
    return null;
  }
}

/** Adapt a raw-payload kickoff/resume to a root Handler. */
function payloadHandler(fn: (raw: Buffer | string) => Promise<void>) {
  return async (e: Envelope): Promise<void> => {
    await fn(e.payload);
  };
}

/** Hand a check_run event to every engine; each no-ops unless its check matches. */
function ciResumeHandler(engines: Engine[]) {
  return async (e: Envelope): Promise<void> => {
    const errors: unknown[] = [];
    for (const eng of engines) {
      try {
        await eng.resume(e.payload);
      } catch (err) {
        errors.push(err);
      }
    }
    if (errors.length > 0) {
      throw new AggregateError(errors, 'ci resume failed');
    }
  };
}

async function run(): Promise<void> {
  try {
    process.loadEnvFile('.env'); // load .env if present; the real environment still wins
  } catch {
    // no .env file — fine
  }
  const cfg = load();

  const llm = buildLLM(cfg);
  const codeLlm = buildCodeLLM(cfg);
  const gh = new Client(cfg.githubToken);
  const notifier = buildNotifier(cfg);

  const summaryAgent = buildSummary(cfg, llm, gh, notifier);

  // Fix engines (event-driven; work without a notifier — they just won't post results).
  const fixDeps: FixDeps = {
    llm,
    codeLlm,
    gh,
    notify: notifier,
    token: cfg.githubToken,
    maxIter: cfg.maxIterations,
    ciTimeoutMs: cfg.ciTimeoutMs,
    log,
  };
  const lintEngine = newLintEngine(fixDeps);
  const covEngine = newCoverageEngine(fixDeps);
  const engines = [lintEngine, covEngine];

  const dispatcher = buildRootDispatcher({
    summaryAgent,
    lintKickoff: payloadHandler((raw) => lintEngine.kickoff(raw)),
    coverageKickoff: payloadHandler((raw) => covEngine.kickoff(raw)),
    ciResume: ciResumeHandler(engines),
    log,
  });

  const safeDispatch = async (e: Envelope): Promise<void> => {
    try {
      await dispatcher.dispatch(e);
    } catch (err) {
      log.error(`dispatch failed: kind=${e.kind} err=${(err as Error).message}`);
    }
  };

  // Scheduler: croner fires on the event loop; dispatch in the background.
  const sched = new Scheduler((e) => void safeDispatch(e));
  sched.add(cfg.cronDaily, Kind.CronDaily);
  sched.add(cfg.cronWeekly, Kind.CronWeekly);

  // Webhooks enqueue asynchronously and return fast.
  const srv = new Server(async (e) => void safeDispatch(e), { secret: cfg.githubWebhookSecret });

  sched.start();
  const httpServer = srv.app.listen(Number(cfg.port), '0.0.0.0', () => {
    log.info('automation-agent listening', {
      port: cfg.port,
      llmProvider: cfg.llmProvider,
      repos: cfg.repos.length,
      notify: cfg.notifyProvider,
      summaryEnabled: summaryAgent !== null,
    });
  });

  const shutdown = (): void => {
    log.info('shutting down');
    sched.stop();
    httpServer.close();
  };
  process.on('SIGINT', shutdown);
  process.on('SIGTERM', shutdown);
}

run().catch((err) => {
  emit('ERROR', `fatal: ${(err as Error).message}`);
  process.exitCode = 1;
});
