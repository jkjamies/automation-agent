/**
 * Runtime configuration for automation-agent, loaded from the environment.
 *
 * This module is the single source of truth for settings; no other module should
 * read `process.env` directly. See .agents/standards/architecture-design.md §12.
 */

import { execFileSync } from 'node:child_process';

/** Looks up an environment variable, returning undefined when unset. */
export type Lookup = (key: string) => string | undefined;

/** Selects which LLM backend agents use. */
export const Provider = {
  Ollama: 'ollama',
  Gemini: 'gemini',
} as const;
export type Provider = (typeof Provider)[keyof typeof Provider];

/** Selects where summaries are posted. */
export const NotifyProvider = {
  Slack: 'slack',
  Teams: 'teams',
} as const;
export type NotifyProvider = (typeof NotifyProvider)[keyof typeof NotifyProvider];

/**
 * Selects where the suspend/resume session and its park record (`prKey → session,
 * attempts, params`) are stored.
 *
 * - `memory` (default): in-process; a restart drops parked runs.
 * - `sqlite`: a durable local file; parked runs survive a restart.
 * - `firestore`: cloud-backed; for the serverless, scale-to-zero deployment.
 */
export const SessionBackend = {
  Memory: 'memory',
  Sqlite: 'sqlite',
  Firestore: 'firestore',
} as const;
export type SessionBackend = (typeof SessionBackend)[keyof typeof SessionBackend];

/** All runtime settings. */
export interface Config {
  // LLM
  llmProvider: Provider;
  ollamaHost: string;
  ollamaModel: string; // default model: triage, explore, summary
  geminiModel: string;
  // Code model: the (typically larger) model used for the code-change steps
  // (lint rewrite, coverage test generation). Falls back to the default model.
  ollamaCodeModel: string;
  geminiCodeModel: string;

  // GitHub / repos
  repos: string[];
  githubToken: string;

  // Notifications
  notifyProvider: NotifyProvider;
  slackWebhookUrl: string;
  teamsWebhookUrl: string;

  // Server / schedule
  port: string;
  cronDaily: string;
  cronWeekly: string;

  // Lint-fixer
  maxIterations: number;
  // ciTimeoutMs bounds how long a suspended fix run waits for its CI result before
  // it is resumed with a timeout outcome (notify + stop). Per-run timer, not a scan.
  ciTimeoutMs: number;
  githubWebhookSecret: string;

  // Sessions (durable suspend/resume)
  // sessionBackend selects where the session + park record live (memory|sqlite|firestore).
  sessionBackend: SessionBackend;
  // sqliteDsn is the sqlite file path used by both the ADK session service and the
  // park store when sessionBackend === 'sqlite' (a plain path, not a URI).
  sqliteDsn: string;
  // firestoreProject is the GCP project for the firestore backend; '' detects it from
  // ADC / GOOGLE_CLOUD_PROJECT / the emulator env.
  firestoreProject: string;
  // firestoreCollection is the root collection prefix for sessions and parked runs.
  firestoreCollection: string;

  // Ingress / auth
  // internalToken is the Bearer token guarding the /internal/* cron + sweep routes;
  // '' leaves those routes disabled (404).
  internalToken: string;
}

/** Read configuration from the process environment, applying defaults. */
export function load(): Config {
  const cfg = loadFrom((key) => process.env[key]);
  // When neither GITHUB_TOKEN nor GH_TOKEN is set, fall back to the developer's gh
  // CLI login so a local run authenticates to GitHub without a hand-set token.
  if (cfg.githubToken === '') {
    cfg.githubToken = ghCliToken();
  }
  return cfg;
}

/**
 * Return the token from `gh auth token`, or '' if the gh CLI is missing,
 * unauthenticated, or errors. This is the one place config shells out rather than
 * reading the environment; it exists so local runs reuse an existing gh login. The
 * short timeout guards against a hung subprocess stalling startup.
 */
function ghCliToken(): string {
  try {
    return execFileSync('gh', ['auth', 'token'], {
      encoding: 'utf8',
      timeout: 5000,
      stdio: ['ignore', 'pipe', 'ignore'],
    }).trim();
  } catch {
    return '';
  }
}

/**
 * Build a Config from an arbitrary lookup func, which keeps {@link load} testable
 * without mutating the real environment.
 *
 * @throws Error on an unparseable MAX_ITERATIONS / CI_TIMEOUT or a failed validate.
 */
export function loadFrom(get: Lookup): Config {
  const rawMax = getOr(get, 'MAX_ITERATIONS', '3');
  const maxIterations = Number.parseInt(rawMax, 10);
  if (!/^[+-]?\d+$/.test(rawMax.trim()) || Number.isNaN(maxIterations)) {
    throw new Error(`MAX_ITERATIONS: invalid integer ${JSON.stringify(rawMax)}`);
  }

  const cfg: Config = {
    llmProvider: getOr(get, 'LLM_PROVIDER', Provider.Ollama) as Provider,
    ollamaHost: getOr(get, 'OLLAMA_HOST', 'http://localhost:11434'),
    ollamaModel: getOr(get, 'OLLAMA_MODEL', 'gemma4:12b'),
    ollamaCodeModel: getOr(get, 'OLLAMA_CODE_MODEL', 'gemma4:26b'),
    geminiModel: getOr(get, 'GEMINI_MODEL', ''),
    geminiCodeModel: getOr(get, 'GEMINI_CODE_MODEL', ''),
    repos: splitList(getOr(get, 'REPOS', '')),
    githubToken: getOr(get, 'GITHUB_TOKEN', getOr(get, 'GH_TOKEN', '')),
    notifyProvider: getOr(get, 'NOTIFY_PROVIDER', NotifyProvider.Slack) as NotifyProvider,
    slackWebhookUrl: getOr(get, 'SLACK_WEBHOOK_URL', ''),
    teamsWebhookUrl: getOr(get, 'TEAMS_WEBHOOK_URL', ''),
    port: getOr(get, 'PORT', '8080'),
    cronDaily: getOr(get, 'CRON_DAILY', '0 9 * * *'),
    cronWeekly: getOr(get, 'CRON_WEEKLY', '0 9 * * 1'),
    maxIterations,
    ciTimeoutMs: parseDuration(getOr(get, 'CI_TIMEOUT', '90m')),
    githubWebhookSecret: getOr(get, 'GITHUB_WEBHOOK_SECRET', ''),
    sessionBackend: getOr(get, 'SESSION_BACKEND', SessionBackend.Memory) as SessionBackend,
    sqliteDsn: getOr(get, 'SQLITE_DSN', 'automation-agent.db'),
    firestoreProject: getOr(get, 'FIRESTORE_PROJECT', ''),
    firestoreCollection: getOr(get, 'FIRESTORE_COLLECTION', 'automation_agent'),
    internalToken: getOr(get, 'INTERNAL_TOKEN', ''),
  };

  // Code models default to the base models when unset.
  if (cfg.ollamaCodeModel === '') {
    cfg.ollamaCodeModel = cfg.ollamaModel;
  }
  if (cfg.geminiCodeModel === '') {
    cfg.geminiCodeModel = cfg.geminiModel;
  }

  validate(cfg);
  return cfg;
}

/**
 * Check invariants that defaults alone cannot guarantee.
 *
 * @throws Error if a provider enum is invalid or maxIterations < 1.
 */
export function validate(c: Config): void {
  if (c.llmProvider !== Provider.Ollama && c.llmProvider !== Provider.Gemini) {
    throw new Error(`invalid LLM_PROVIDER ${JSON.stringify(c.llmProvider)} (want ollama|gemini)`);
  }
  if (c.notifyProvider !== NotifyProvider.Slack && c.notifyProvider !== NotifyProvider.Teams) {
    throw new Error(
      `invalid NOTIFY_PROVIDER ${JSON.stringify(c.notifyProvider)} (want slack|teams)`,
    );
  }
  if (c.maxIterations < 1) {
    throw new Error(`MAX_ITERATIONS must be >= 1, got ${c.maxIterations}`);
  }
  if (
    c.sessionBackend !== SessionBackend.Memory &&
    c.sessionBackend !== SessionBackend.Sqlite &&
    c.sessionBackend !== SessionBackend.Firestore
  ) {
    throw new Error(
      `invalid SESSION_BACKEND ${JSON.stringify(c.sessionBackend)} (want memory|sqlite|firestore)`,
    );
  }
  const port = Number.parseInt(c.port, 10);
  if (!/^[+-]?\d+$/.test(c.port.trim()) || Number.isNaN(port)) {
    throw new Error(`PORT must be numeric, got ${JSON.stringify(c.port)}`);
  }
  if (port < 1 || port > 65535) {
    throw new Error(`PORT must be in 1..65535, got ${port}`);
  }
}

function getOr(get: Lookup, key: string, def: string): string {
  const v = get(key);
  if (v !== undefined && v !== '') {
    return v;
  }
  return def;
}

function splitList(s: string): string[] {
  if (s.trim() === '') {
    return [];
  }
  return s
    .split(',')
    .map((p) => p.trim())
    .filter((p) => p !== '');
}

// Duration unit table (subset that matters for CI_TIMEOUT), in milliseconds.
const DURATION_UNITS_MS: Record<string, number> = {
  ns: 1e-6,
  us: 1e-3,
  'µs': 1e-3,
  ms: 1,
  s: 1000,
  m: 60_000,
  h: 3_600_000,
};

/**
 * Parse a duration string (e.g. `90m`, `1h30m`) into milliseconds.
 * Supports the unit subset CI_TIMEOUT needs (ns, us, ms, s, m, h).
 *
 * @throws Error if the string is empty or malformed.
 */
export function parseDuration(s: string): number {
  const text = s.trim();
  if (text === '') {
    throw new Error('CI_TIMEOUT: empty duration');
  }
  // Repeated units (e.g. "90m90m") are summed, matching Go's time.ParseDuration leniency
  // (the reference) — intentional parity, not a bug.
  const re = /(\d+(?:\.\d+)?)(ns|us|µs|ms|s|m|h)/g;
  const matches = [...text.matchAll(re)];
  const consumed = matches.map((m) => m[1]! + m[2]!).join('');
  if (matches.length === 0 || consumed !== text) {
    throw new Error(`CI_TIMEOUT: invalid duration ${JSON.stringify(s)}`);
  }
  return matches.reduce((acc, m) => acc + Number.parseFloat(m[1]!) * DURATION_UNITS_MS[m[2]!]!, 0);
}
