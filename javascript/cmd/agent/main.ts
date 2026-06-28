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
import { StaticProvider, newAppProvider, type TokenProvider } from '../../src/auth/auth';
import { type Config, TasksBackend, appMode, load } from '../../src/config/config';
import { Client } from '../../src/githubapi/client';
import { sshCommand } from '../../src/gitrepo/repo';
import { type Envelope } from '../../src/ingest/envelope';
import { type Notifier, newNotifier } from '../../src/notify/notify';
import { type DispatchFunc, InProcess, type Transport, newCloudTasks } from '../../src/tasks/index';
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

/**
 * Pick the GitHub auth provider from config: an App installation-token provider when App
 * mode is configured (the production path), else a static PAT/anonymous provider. The
 * provider backs both the REST client and the git transport, so they share one credential.
 */
function buildTokenProvider(cfg: Config): TokenProvider {
  if (appMode(cfg)) {
    return newAppProvider(cfg.githubAppId, cfg.githubAppInstallationId, cfg.githubAppPrivateKeyPem);
  }
  return new StaticProvider(cfg.githubToken);
}

/**
 * Select the webhook execution transport: Cloud Tasks in production (durable, in-request,
 * rate-limited by the queue) or the in-process task pool for local dev (the default). See
 * `specs/20260626-workflow-execution-transport.md`.
 */
function buildTransport(cfg: Config, dispatch: DispatchFunc): Transport {
  if (cfg.tasksBackend === TasksBackend.CloudTasks) {
    log.info('execution transport: cloud tasks', {
      project: cfg.tasksProject,
      location: cfg.tasksLocation,
      queue: cfg.tasksQueue,
      dispatchUrl: cfg.dispatchUrl,
    });
    return newCloudTasks(
      cfg.tasksProject,
      cfg.tasksLocation,
      cfg.tasksQueue,
      cfg.dispatchUrl,
      cfg.internalToken,
      cfg.tasksDispatchDeadlineMs,
    );
  }
  log.info('execution transport: in-process (local/default)');
  return new InProcess(dispatch, log, MAX_CONCURRENT_DISPATCH);
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

// maxConcurrentDispatch bounds in-flight in-process dispatches (backpressure under burst); the
// drain timeout lives inside the in-process transport.
const MAX_CONCURRENT_DISPATCH = 32;

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

  // Honor GIT_SSH_KEY for the shell-out git: export GIT_SSH_COMMAND so every child git
  // (clone/push) pins its ssh transport to the explicit key while still inheriting the full
  // environment (PATH/HOME/known_hosts). Done here at the composition root rather than via
  // simple-git's per-call .env() — that replaces the child environment and would strip PATH,
  // breaking git's lookup of the ssh binary — and keeps `src/` free of process.env access.
  const gitSshCommand = sshCommand(cfg.gitSshKey);
  if (gitSshCommand !== '') {
    process.env.GIT_SSH_COMMAND = gitSshCommand;
  }

  const llm = buildLLM(cfg);
  const codeLlm = buildCodeLLM(cfg);
  // One auth provider backs both the REST client and the git transport (PAT/anonymous or a
  // GitHub App installation token), so they share one credential.
  const provider = buildTokenProvider(cfg);
  const gh = new Client(provider);
  // SSH only authenticates the git transport (clone/push). The GitHub REST API — opening
  // and labeling PRs, reading the CI check — still needs a token (or `gh` login). Warn
  // rather than fail so read-only/dry-run flows still work, but PR operations will not.
  // App mode supplies its own REST credential, so the warning is PAT-mode only.
  if (cfg.gitTransport === 'ssh' && cfg.githubToken === '' && !appMode(cfg)) {
    log.warn(
      'GIT_TRANSPORT=ssh but no GitHub token found (GITHUB_TOKEN/GH_TOKEN/`gh auth token`); git clone+push will use ssh, but PR operations against the REST API will fail — run `gh auth login` or set a token',
    );
  }
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
    provider,
    gitTransport: cfg.gitTransport,
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

  // Webhooks enqueue asynchronously and return fast. The transport runs the dispatch:
  // in-process (default) on a bounded task pool drained on SIGTERM, or — in production — via
  // Cloud Tasks, which delivers each envelope to /internal/dispatch so the compute runs
  // in-request (CPU stays allocated) with durable retry. (With the default `memory` session
  // backend, parked runs are not durable, so a restart strands them; a sqlite/firestore
  // backend survives restart.) See specs/20260626-workflow-execution-transport.md.
  const transport = buildTransport(cfg, (e: Envelope) => dispatcher.dispatch(e));

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
  const srv = new Server(
    async (e) => {
      await transport.enqueue(e);
    },
    {
      secret: cfg.githubWebhookSecret,
      internalToken: cfg.internalToken,
      sweep,
      // /internal/dispatch executes a queued envelope in-request (the Cloud Tasks worker).
      dispatch: (e) => dispatcher.dispatch(e),
      log,
    },
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

  let shuttingDown = false;
  const shutdown = async (): Promise<void> => {
    if (shuttingDown) {
      return;
    }
    shuttingDown = true;
    log.info('shutting down');
    await new Promise<void>((resolve) => httpServer.close(() => resolve()));
    // Close the transport after the server stops accepting: the in-process backend drains
    // in-flight dispatches (bounded), the Cloud Tasks backend closes its client. Done before
    // the park-store close so any draining dispatch still has its store.
    await transport.close();
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
