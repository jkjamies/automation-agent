/**
 * The automation-agent service entrypoint.
 *
 * Wires configuration, tooling, agents, and the webhook server together, then runs until
 * interrupted. Composition only — logic lives in `src/`.
 *
 * Run with `tsx cmd/agent/main.ts` (or `make run`).
 */
import type { BaseAgent } from '@google/adk';

import { newCoverageEngine } from '../../src/agent/covfixer/index';
import { type Deps as FixDeps, type Engine } from '../../src/agent/fixflow/index';
import { newLintEngine } from '../../src/agent/lintfixer/index';
import { buildRootDispatcher } from '../../src/agent/root/agentsSetup';
import { buildLLM, buildCodeLLM } from '../../src/agent/setup/llm';
import { newParkStore } from '../../src/agent/setup/parkstore';
import { newSessionService } from '../../src/agent/setup/session';
import { buildSummaryAgent } from '../../src/agent/summary/agentsSetup';
import type { CommitLister } from '../../src/agent/summary/summary';
import { type Config, load } from '../../src/config/config';
import { Client } from '../../src/githubapi/client';
import { type Envelope } from '../../src/ingest/envelope';
import { type Notifier, newNotifier } from '../../src/notify/notify';
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

/**
 * Build a summary workflow agent for the given commit window and notification title, or
 * null if it can't be fully configured (no repos or no notifier).
 */
function buildSummary(
  cfg: Config,
  llm: ReturnType<typeof buildLLM>,
  gh: CommitLister,
  notifier: Notifier | null,
  windowMs: number,
  title: string,
): BaseAgent | null {
  if (cfg.repos.length === 0) {
    log.warn('no REPOS configured; summary workflow disabled');
    return null;
  }
  if (!notifier) {
    return null; // buildNotifier already warned
  }
  try {
    return buildSummaryAgent({ llm, gh, notify: notifier, repos: cfg.repos, windowMs, title });
  } catch (err) {
    log.warn(`summary workflow disabled: ${(err as Error).message}`);
    return null;
  }
}

const DAY_MS = 24 * 60 * 60 * 1000;

// maxConcurrentDispatch bounds in-flight webhook/cron dispatches; drainTimeoutMs caps how
// long shutdown waits for in-flight dispatches to finish before exiting anyway.
const MAX_CONCURRENT_DISPATCH = 32;
const DRAIN_TIMEOUT_MS = 15_000;

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

  // One session service + park store, shared by both fix engines (namespaced by app name).
  // memory (the default) keeps today's behavior; the durable backends persist parked runs
  // across restarts.
  const sessionService = newSessionService(cfg);
  const parkStore = newParkStore(cfg);

  // Summary workflow (needs repos + a notifier). The daily Cloud Scheduler trigger fires it.
  const summaryDaily = buildSummary(cfg, llm, gh, notifier, DAY_MS, 'Daily commit digest');
  // /internal/cron/daily is the only daily-digest trigger, and it 404s when INTERNAL_TOKEN
  // is unset. Warn rather than fail silently so a built-but-unreachable digest is visible.
  if (summaryDaily !== null && cfg.internalToken === '') {
    log.warn(
      'daily summary built but INTERNAL_TOKEN is unset; /internal/cron/daily is disabled (404), so the digest cannot be triggered',
    );
  }

  // Fix engines (event-driven; work without a notifier — they just won't post results).
  const fixDeps: FixDeps = {
    llm,
    codeLlm,
    gh,
    notify: notifier,
    token: cfg.githubToken,
    repos: cfg.repos,
    maxIter: cfg.maxIterations,
    ciTimeoutMs: cfg.ciTimeoutMs,
    log,
    prLabel: cfg.agentPrLabel,
    sessionService,
    parkStore,
  };
  const lintEngine = newLintEngine(fixDeps);
  const covEngine = newCoverageEngine(fixDeps);
  const engines = [lintEngine, covEngine];

  const dispatcher = buildRootDispatcher({
    summaryDaily,
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

  // Bounded, drainable dispatch pool. Webhook dispatches acquire a permit before the 202
  // (backpressure under burst); every dispatch is tracked so a SIGTERM drains in-flight
  // work instead of dropping it. (With the default `memory` backend, parked runs are not
  // durable, so a restart strands them; a sqlite/firestore backend survives restart.)
  let permits = MAX_CONCURRENT_DISPATCH;
  const permitWaiters: Array<() => void> = [];
  const acquire = (): Promise<void> => {
    if (permits > 0) {
      permits -= 1;
      return Promise.resolve();
    }
    return new Promise<void>((resolve) => permitWaiters.push(resolve));
  };
  const release = (): void => {
    const next = permitWaiters.shift();
    if (next) {
      next(); // hand the permit directly to the next waiter
    } else {
      permits += 1;
    }
  };
  const inFlight = new Set<Promise<void>>();
  const track = (p: Promise<void>): void => {
    inFlight.add(p);
    void p.finally(() => inFlight.delete(p));
  };

  // The durable timeout catch-all behind POST /internal/sweep: resolve every engine's
  // parked runs whose CI never reported (Cloud Scheduler drives it on a schedule). One
  // engine's failure must not stop the others — a stuck run on another engine still needs
  // freeing — so collect-and-continue (like ciResumeHandler), then surface so the handler
  // 500s and Cloud Scheduler retries.
  const sweep = async (): Promise<void> => {
    const errors: unknown[] = [];
    for (const eng of engines) {
      try {
        await eng.sweepTimeouts();
      } catch (err) {
        log.error(`sweep failed for an engine: ${(err as Error).message}`);
        errors.push(err);
      }
    }
    if (errors.length > 0) {
      throw new AggregateError(errors, 'sweep failed');
    }
  };

  if (!cfg.githubWebhookSecret) {
    emit(
      'WARN',
      'GITHUB_WEBHOOK_SECRET is unset — webhook signatures are NOT verified; the /webhooks/github route accepts unauthenticated requests (dev only)',
    );
  }
  // Webhooks enqueue asynchronously and return fast; a permit bounds concurrency.
  const srv = new Server(
    async (e) => {
      await acquire();
      track(safeDispatch(e).finally(release));
    },
    { secret: cfg.githubWebhookSecret, internalToken: cfg.internalToken, sweep },
  );

  const httpServer = srv.app.listen(Number(cfg.port), '0.0.0.0', () => {
    log.info('automation-agent listening', {
      port: cfg.port,
      llmProvider: cfg.llmProvider,
      repos: cfg.repos.length,
      notify: cfg.notifyProvider,
      summaryEnabled: summaryDaily !== null,
    });
  });
  // HTTP server timeouts (the http.Server analogue of Go's ReadHeaderTimeout / ReadTimeout
  // / IdleTimeout) to blunt Slowloris and stalled connections.
  httpServer.headersTimeout = 10_000;
  httpServer.requestTimeout = 30_000;
  httpServer.keepAliveTimeout = 120_000;

  // drain waits for in-flight dispatches to finish, bounded by DRAIN_TIMEOUT_MS, so a
  // clean SIGTERM completes work in flight rather than abandoning it.
  const drain = async (): Promise<void> => {
    if (inFlight.size === 0) {
      return;
    }
    const done = Promise.allSettled([...inFlight]).then(() => 'done' as const);
    const timeout = new Promise<'timeout'>((resolve) => {
      const t = setTimeout(() => resolve('timeout'), DRAIN_TIMEOUT_MS);
      t.unref?.();
    });
    if ((await Promise.race([done, timeout])) === 'timeout') {
      log.warn('drain timed out; exiting with work still in flight');
    } else {
      log.info('drained in-flight work');
    }
  };

  let shuttingDown = false;
  const shutdown = async (): Promise<void> => {
    if (shuttingDown) {
      return;
    }
    shuttingDown = true;
    log.info('shutting down');
    await new Promise<void>((resolve) => httpServer.close(() => resolve()));
    await drain();
    // Release a durable park store's backing connection (a no-op for the memory backend).
    await parkStore.close();
  };
  process.on('SIGINT', () => void shutdown());
  process.on('SIGTERM', () => void shutdown());
}

run().catch((err) => {
  emit('ERROR', `fatal: ${(err as Error).message}`);
  process.exitCode = 1;
});
