/**
 * Runtime configuration for automation-agent, loaded from the environment.
 *
 * This module is the single source of truth for settings; no other module should
 * read `process.env` directly. See .agents/standards/architecture-design.md §12.
 */

import { execFileSync } from 'node:child_process';
import { createPrivateKey } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { inspect } from 'node:util';

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

/**
 * Selects the webhook execution transport: how an enqueued envelope reaches the dispatcher.
 *
 * - `inprocess` (default): a background promise pool that drains on SIGTERM — the pre-transport
 *   behavior, for local dev. Not durable; a reclaim loses in-flight work.
 * - `cloudtasks`: a Cloud Tasks queue POSTs each envelope to /internal/dispatch so the workflow
 *   runs in-request (CPU stays allocated) with durable retry — the production path.
 */
export const TasksBackend = {
  InProcess: 'inprocess',
  CloudTasks: 'cloudtasks',
} as const;
export type TasksBackend = (typeof TasksBackend)[keyof typeof TasksBackend];

/**
 * Selects the OpenTelemetry trace sink. The app speaks exactly these four and never names a
 * vendor: any OTLP-native backend is reached with `otlp` + an endpoint. See
 * .agents/standards/observability.md.
 *
 * - `none` (default): tracing is off; nothing is registered and the service is unchanged.
 * - `console`: spans as text on stdout (local dev, the playground).
 * - `otlp`: OTLP/HTTP to `OTEL_EXPORTER_OTLP_ENDPOINT` (any OTLP backend or a Collector).
 * - `gcp`: Google Cloud Trace via Application Default Credentials.
 */
export const TracesExporter = {
  None: 'none',
  Console: 'console',
  Otlp: 'otlp',
  Gcp: 'gcp',
} as const;
export type TracesExporter = (typeof TracesExporter)[keyof typeof TracesExporter];

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
  // GitHub App (production auth). githubAppId === 0 means App mode is off and the static
  // githubToken (PAT) is used. See appMode() and specs/20260625-github-app-authentication.md.
  // githubAppPrivateKeyPem holds the literal PEM (from the env var or the key file),
  // unescaped and validated to parse as RSA at load time.
  githubAppId: number;
  githubAppInstallationId: number;
  githubAppPrivateKeyPem: string;
  // gitTransport selects the git clone/push transport: 'https' (default — uses githubToken)
  // or 'ssh' (local dev — ssh-agent/keys). SSH only covers the git transport; the GitHub
  // REST API (open/label PR, read CI) still needs a token, so an ssh run without a token
  // warns at startup.
  gitTransport: string;
  // gitSshKey is an explicit private-key path for gitTransport=ssh (GIT_SSH_KEY); empty
  // falls back to ssh-agent then the default identity files.
  gitSshKey: string;

  // Notifications
  notifyProvider: NotifyProvider;
  slackWebhookUrl: string;
  teamsWebhookUrl: string;

  // Server
  port: string;

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

  // agentPrLabel is the single human-facing label applied to every agent PR on creation
  // (AGENT_PR_LABEL). Write-only: PR lookup is by branch, so the label never gates behavior.
  agentPrLabel: string;

  // Execution transport (webhook → dispatcher). tasksBackend selects in-process (default) or
  // Cloud Tasks; the Cloud Tasks settings locate the queue and the worker endpoint.
  tasksBackend: TasksBackend;
  // tasksProject is the GCP project owning the queue (TASKS_PROJECT); empty falls back to
  // GOOGLE_CLOUD_PROJECT. Required for cloudtasks.
  tasksProject: string;
  // tasksLocation is the queue's region (TASKS_LOCATION, e.g. 'us-central1'). Required for
  // cloudtasks.
  tasksLocation: string;
  // tasksQueue is the Cloud Tasks queue name (TASKS_QUEUE). Required for cloudtasks.
  tasksQueue: string;
  // dispatchUrl is the full URL of the /internal/dispatch worker the queue POSTs to
  // (DISPATCH_URL, e.g. https://agent-xyz.run.app/internal/dispatch). Required for cloudtasks.
  dispatchUrl: string;
  // tasksDispatchDeadlineMs is how long Cloud Tasks waits for an /internal/dispatch attempt
  // before cancelling and retrying it (TASKS_DISPATCH_DEADLINE). The HTTP-target default is
  // only 10m, so a long workflow would be cancelled mid-run and retried, duplicating side
  // effects. Cloud Tasks caps this at 30m, which is therefore the default and the ceiling.
  tasksDispatchDeadlineMs: number;

  // Observability (OpenTelemetry tracing). Off by default (otelTracesExporter=none): nothing is
  // registered and the service is unchanged. config is the single place that reads OTEL_*; the obs
  // package takes these as a typed struct. See obs and .agents/standards/observability.md.
  // otelTracesExporter selects the sink: none | console | otlp | gcp.
  otelTracesExporter: TracesExporter;
  // otelTracesExporterSet records whether OTEL_TRACES_EXPORTER was explicitly provided in the
  // environment (vs. defaulted). The playground uses it to decide whether to override with its
  // console default without itself reading the environment.
  otelTracesExporterSet: boolean;
  // otelServiceName is the resource service.name on every span (OTEL_SERVICE_NAME).
  otelServiceName: string;
  // otelExporterOtlpEndpoint / otelExporterOtlpHeaders configure the otlp exporter (standard
  // OTEL_EXPORTER_OTLP_ENDPOINT / OTEL_EXPORTER_OTLP_HEADERS). The endpoint is required for the otlp
  // exporter; the headers are a secret (masked in the config log view).
  otelExporterOtlpEndpoint: string;
  otelExporterOtlpHeaders: string;
  // otelTracesSampler is a standard OTEL_TRACES_SAMPLER value; the default parentbased_always_on
  // records every locally-started trace and honors an upstream decision.
  otelTracesSampler: string;
  // otelCaptureMessageContent gates whether prompt/response bodies are captured as span content —
  // sensitive (reviewed source code), so off by default. Its name is the standard GenAI-semconv var
  // (OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT) the framework reads natively, so surfacing
  // it here keeps it under one config surface. (Body-capture wiring itself is a follow-up.)
  otelCaptureMessageContent: boolean;
}

/** Config fields whose value is a credential and must never appear in a log. */
const SECRET_KEYS = [
  'githubToken',
  'githubWebhookSecret',
  'internalToken',
  'slackWebhookUrl',
  'teamsWebhookUrl',
  'githubAppPrivateKeyPem',
  // OTLP headers commonly carry a vendor API key (OTEL_EXPORTER_OTLP_HEADERS).
  'otelExporterOtlpHeaders',
] as const satisfies readonly (keyof Config)[];

/**
 * Attach a custom inspect hook so console.log / util.inspect of the config masks every
 * credential — an unset secret stays visibly empty, a set one collapses to '***'. The hook
 * is non-enumerable, so it never affects property access, spreads, or JSON serialization.
 */
function withRedactedInspect(cfg: Config): Config {
  Object.defineProperty(cfg, inspect.custom, {
    enumerable: false,
    value(): Record<string, unknown> {
      const masked: Record<string, unknown> = { ...cfg };
      for (const key of SECRET_KEYS) {
        if (masked[key]) {
          masked[key] = '***';
        }
      }
      return masked;
    },
  });
  return cfg;
}

/** Read configuration from the process environment, applying defaults. */
export function load(): Config {
  const cfg = loadFrom((key) => process.env[key]);
  // When neither GITHUB_TOKEN nor GH_TOKEN is set, fall back to the developer's gh
  // CLI login so a local run authenticates to GitHub without a hand-set token. Skipped
  // in App mode: the App provider mints its own tokens, so shelling out to gh would be a
  // useless subprocess that could also hydrate a PAT the deployment never asked for.
  if (!appMode(cfg) && cfg.githubToken === '') {
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
    // App credentials are resolved below (resolveGithubApp); absent App vars leave these
    // zero values (PAT mode).
    githubAppId: 0,
    githubAppInstallationId: 0,
    githubAppPrivateKeyPem: '',
    gitTransport: getOr(get, 'GIT_TRANSPORT', 'https'),
    gitSshKey: getOr(get, 'GIT_SSH_KEY', ''),
    notifyProvider: getOr(get, 'NOTIFY_PROVIDER', NotifyProvider.Slack) as NotifyProvider,
    slackWebhookUrl: getOr(get, 'SLACK_WEBHOOK_URL', ''),
    teamsWebhookUrl: getOr(get, 'TEAMS_WEBHOOK_URL', ''),
    port: getOr(get, 'PORT', '8080'),
    maxIterations,
    ciTimeoutMs: parseDuration(getOr(get, 'CI_TIMEOUT', '90m')),
    githubWebhookSecret: getOr(get, 'GITHUB_WEBHOOK_SECRET', ''),
    sessionBackend: getOr(get, 'SESSION_BACKEND', SessionBackend.Memory) as SessionBackend,
    sqliteDsn: getOr(get, 'SQLITE_DSN', 'automation-agent.db'),
    firestoreProject: getOr(get, 'FIRESTORE_PROJECT', ''),
    firestoreCollection: getOr(get, 'FIRESTORE_COLLECTION', 'automation_agent'),
    internalToken: getOr(get, 'INTERNAL_TOKEN', ''),
    agentPrLabel: getOr(get, 'AGENT_PR_LABEL', 'automation-agent'),
    tasksBackend: getOr(get, 'TASKS_BACKEND', TasksBackend.InProcess) as TasksBackend,
    // TASKS_PROJECT falls back to GOOGLE_CLOUD_PROJECT (the ambient Cloud Run var).
    tasksProject: getOr(get, 'TASKS_PROJECT', getOr(get, 'GOOGLE_CLOUD_PROJECT', '')),
    tasksLocation: getOr(get, 'TASKS_LOCATION', ''),
    tasksQueue: getOr(get, 'TASKS_QUEUE', ''),
    dispatchUrl: getOr(get, 'DISPATCH_URL', ''),
    tasksDispatchDeadlineMs: parseDuration(
      getOr(get, 'TASKS_DISPATCH_DEADLINE', '30m'),
      'TASKS_DISPATCH_DEADLINE',
    ),
    otelTracesExporter: getOr(get, 'OTEL_TRACES_EXPORTER', TracesExporter.None) as TracesExporter,
    // Whether OTEL_TRACES_EXPORTER was explicitly provided (a non-blank value), so the playground
    // can override the default without reading the environment itself.
    otelTracesExporterSet: (get('OTEL_TRACES_EXPORTER') ?? '').trim() !== '',
    otelServiceName: getOr(get, 'OTEL_SERVICE_NAME', 'automation-agent'),
    otelExporterOtlpEndpoint: getOr(get, 'OTEL_EXPORTER_OTLP_ENDPOINT', ''),
    otelExporterOtlpHeaders: getOr(get, 'OTEL_EXPORTER_OTLP_HEADERS', ''),
    otelTracesSampler: getOr(get, 'OTEL_TRACES_SAMPLER', 'parentbased_always_on'),
    otelCaptureMessageContent: getBool(get, 'OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT', false),
  };

  // Code models default to the base models when unset.
  if (cfg.ollamaCodeModel === '') {
    cfg.ollamaCodeModel = cfg.ollamaModel;
  }
  if (cfg.geminiCodeModel === '') {
    cfg.geminiCodeModel = cfg.geminiModel;
  }

  // Resolve GitHub App credentials (production auth path). Absent App vars leave the zero
  // values — PAT mode. Partial/misconfigured App vars are a startup error, never a silent
  // fallback to PAT.
  const app = resolveGithubApp(get);
  cfg.githubAppId = app.appId;
  cfg.githubAppInstallationId = app.installationId;
  cfg.githubAppPrivateKeyPem = app.privateKeyPem;

  validate(cfg);
  return withRedactedInspect(cfg);
}

/** Whether GitHub App authentication is configured (the production path). False means the
 * static PAT fallback (githubToken) is used. */
export function appMode(c: Config): boolean {
  return c.githubAppId !== 0;
}

/**
 * Read the `GITHUB_APP_*` vars and decide the auth mode, returning the resolved app id,
 * installation id, and private-key PEM.
 *
 * With none set, returns zeros / '' — PAT mode. With any set, App mode is intended and
 * every requirement is enforced (App ID, a pinned installation id, and exactly one
 * private-key source), so a partial configuration is a startup error, not a silent
 * fallback to PAT.
 *
 * @throws Error on a partial/misconfigured App setup, a non-positive id, an unreadable key
 *   file, or a key that is not valid RSA PEM.
 */
function resolveGithubApp(get: Lookup): {
  appId: number;
  installationId: number;
  privateKeyPem: string;
} {
  const appIdStr = getOr(get, 'GITHUB_APP_ID', '');
  const installIdStr = getOr(get, 'GITHUB_APP_INSTALLATION_ID', '');
  const keyLiteral = getOr(get, 'GITHUB_APP_PRIVATE_KEY', '');
  const keyPath = getOr(get, 'GITHUB_APP_PRIVATE_KEY_PATH', '');

  if (!appIdStr && !installIdStr && !keyLiteral && !keyPath) {
    return { appId: 0, installationId: 0, privateKeyPem: '' }; // PAT mode — no App vars present.
  }

  // Any App var present signals intent to use App mode; require the full set.
  if (!appIdStr) {
    throw new Error('GITHUB_APP_* set but GITHUB_APP_ID is missing (App mode requires GITHUB_APP_ID)');
  }
  if (!installIdStr) {
    throw new Error('App mode requires GITHUB_APP_INSTALLATION_ID (single pinned installation)');
  }
  if (keyLiteral && keyPath) {
    throw new Error(
      'set exactly one of GITHUB_APP_PRIVATE_KEY or GITHUB_APP_PRIVATE_KEY_PATH, not both',
    );
  }
  if (!keyLiteral && !keyPath) {
    throw new Error('App mode requires one of GITHUB_APP_PRIVATE_KEY or GITHUB_APP_PRIVATE_KEY_PATH');
  }

  const appId = positiveId(appIdStr, 'GITHUB_APP_ID');
  const installationId = positiveId(installIdStr, 'GITHUB_APP_INSTALLATION_ID');

  let raw: string;
  if (keyPath) {
    try {
      raw = readFileSync(keyPath, 'utf-8');
    } catch (err) {
      throw new Error(`read GITHUB_APP_PRIVATE_KEY_PATH ${JSON.stringify(keyPath)}: ${errMsg(err)}`);
    }
  } else {
    raw = keyLiteral;
  }
  return { appId, installationId, privateKeyPem: normalizePrivateKeyPem(raw) };
}

/** Parse a positive integer id, rejecting non-numeric and <= 0 (0 would collide with
 * appMode()'s zero-value sentinel and silently fall back to PAT). */
function positiveId(raw: string, name: string): number {
  if (!/^[+-]?\d+$/.test(raw)) {
    throw new Error(`${name} must be numeric, got ${JSON.stringify(raw)}`);
  }
  const n = Number.parseInt(raw, 10);
  if (n <= 0) {
    throw new Error(`${name} must be > 0, got ${n}`);
  }
  return n;
}

/**
 * Make the App private key robust to how it is delivered: CI secret stores often flatten
 * newlines to the literal characters `\n`, so when the value looks like PEM and contains
 * escaped `\n` sequences, restore them — even if a real trailing newline is also present.
 * Then validate the key parses as an RSA private key, failing at startup with a clear
 * message rather than a cryptic RS256 error at the first token exchange.
 *
 * @throws Error if the value is not valid PEM or is not an RSA private key.
 */
function normalizePrivateKeyPem(raw: string): string {
  let pem = raw;
  if (pem.includes('-----BEGIN') && pem.includes('\\n')) {
    pem = pem.replaceAll('\\n', '\n');
  }
  let key;
  try {
    key = createPrivateKey(pem);
  } catch (err) {
    throw new Error(`GitHub App private key is not valid PEM / does not parse: ${errMsg(err)}`);
  }
  // GitHub App keys are RSA, and RS256 JWT signing requires an RSA key — reject EC/Ed25519
  // here rather than failing cryptically at the first token exchange.
  if (key.asymmetricKeyType !== 'rsa') {
    throw new Error(`GitHub App private key must be RSA, got ${key.asymmetricKeyType ?? 'unknown'}`);
  }
  return pem;
}

/** Extract a message from a thrown value. */
function errMsg(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
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
  if (c.gitTransport !== 'https' && c.gitTransport !== 'ssh') {
    throw new Error(`invalid GIT_TRANSPORT ${JSON.stringify(c.gitTransport)} (want https|ssh)`);
  }
  const port = Number.parseInt(c.port, 10);
  if (!/^[+-]?\d+$/.test(c.port.trim()) || Number.isNaN(port)) {
    throw new Error(`PORT must be numeric, got ${JSON.stringify(c.port)}`);
  }
  if (port < 1 || port > 65535) {
    throw new Error(`PORT must be in 1..65535, got ${port}`);
  }
  // In App mode an installation can see every repo it is installed on, so an empty
  // allow-list ("act on all repos", the PAT-mode default) is a footgun — fail fast. PAT
  // mode keeps "empty = all" for local-dev back-compat.
  if (appMode(c) && c.repos.length === 0) {
    throw new Error(
      'REPOS must be set in GitHub App mode (empty REPOS = all repos is rejected to avoid acting on every installed repo)',
    );
  }
  validateTasks(c);
  validateOtel(c);
}

/**
 * Validate the observability settings: the exporter must be one of the four known sinks, and the
 * `otlp` exporter needs an endpoint (else it would silently export nowhere).
 *
 * @throws Error on an unknown exporter or `otlp` with no endpoint.
 */
function validateOtel(c: Config): void {
  if (
    c.otelTracesExporter === TracesExporter.None ||
    c.otelTracesExporter === TracesExporter.Console ||
    c.otelTracesExporter === TracesExporter.Gcp
  ) {
    return;
  }
  if (c.otelTracesExporter === TracesExporter.Otlp) {
    if (c.otelExporterOtlpEndpoint.trim() === '') {
      throw new Error('OTEL_TRACES_EXPORTER=otlp requires OTEL_EXPORTER_OTLP_ENDPOINT');
    }
    return;
  }
  throw new Error(
    `invalid OTEL_TRACES_EXPORTER ${JSON.stringify(c.otelTracesExporter)} (want none|console|otlp|gcp)`,
  );
}

/**
 * Validate the execution-transport settings. The default (`inprocess`) needs nothing; the
 * `cloudtasks` backend FAILS FAST unless every requirement is present, so a production
 * deployment never silently fails to dispatch.
 *
 * @throws Error on an unknown backend or an incomplete/insecure cloudtasks configuration.
 */
function validateTasks(c: Config): void {
  if (c.tasksBackend === TasksBackend.InProcess) {
    return;
  }
  if (c.tasksBackend !== TasksBackend.CloudTasks) {
    throw new Error(`invalid TASKS_BACKEND ${JSON.stringify(c.tasksBackend)} (want inprocess|cloudtasks)`);
  }
  // Cloud Tasks needs the queue coordinates and worker URL, plus the Bearer token the task
  // carries: without INTERNAL_TOKEN, /internal/dispatch is disabled (404) and every task would
  // fail permanently. Fail fast rather than silently never dispatching.
  const missing: string[] = [];
  if (c.tasksProject === '') {
    missing.push('TASKS_PROJECT (or GOOGLE_CLOUD_PROJECT)');
  }
  if (c.tasksLocation === '') {
    missing.push('TASKS_LOCATION');
  }
  if (c.tasksQueue === '') {
    missing.push('TASKS_QUEUE');
  }
  // DISPATCH_URL must be an absolute https URL ending in /internal/dispatch: the Cloud Tasks
  // task carries INTERNAL_TOKEN as a Bearer header to it, so a plaintext http:// target would
  // leak the token in transit, and a base URL or stray path would pass the scheme check then
  // 404 every task at runtime. A suffix match (not equality) tolerates a gateway path prefix.
  if (!isSecureDispatchUrl(c.dispatchUrl)) {
    missing.push('DISPATCH_URL (must be an absolute https:// URL ending in /internal/dispatch)');
  }
  if (c.internalToken === '') {
    missing.push('INTERNAL_TOKEN (the Bearer the task carries to /internal/dispatch)');
  }
  // Cloud Tasks clamps an HTTP-target dispatch deadline to 15s..30m; a value outside that range
  // is silently rejected at CreateTask, so reject it here instead.
  if (c.tasksDispatchDeadlineMs < 15_000 || c.tasksDispatchDeadlineMs > 30 * 60_000) {
    missing.push('TASKS_DISPATCH_DEADLINE (must be between 15s and 30m)');
  }
  // In Cloud Tasks mode the deployment is production-facing, so an unverified webhook surface
  // is a real exposure rather than a dev convenience — require the HMAC secret (it stays an
  // opt-in warning only for the local inprocess default).
  if (c.githubWebhookSecret === '') {
    missing.push('GITHUB_WEBHOOK_SECRET (webhook signatures must be verified in production)');
  }
  if (missing.length > 0) {
    throw new Error(`TASKS_BACKEND=cloudtasks requires ${missing.join(', ')}`);
  }
}

/** Whether `raw` is an absolute https URL whose path ends in /internal/dispatch. */
function isSecureDispatchUrl(raw: string): boolean {
  if (raw === '') {
    return false;
  }
  let u: URL;
  try {
    u = new URL(raw);
  } catch {
    return false;
  }
  return u.protocol === 'https:' && u.host !== '' && u.pathname.endsWith('/internal/dispatch');
}

function getOr(get: Lookup, key: string, def: string): string {
  // Trim so trailing whitespace/newlines on a value from the real environment
  // (e.g. a CI secret with a trailing newline) do not leak into the setting.
  const v = get(key)?.trim();
  if (v !== undefined && v !== '') {
    return v;
  }
  return def;
}

/**
 * Parse a boolean env var. Accepts the common truthy/falsy spellings (1/true/yes/on and
 * 0/false/no/off, case-insensitively); an unset, blank, or unrecognized value uses `def` — the
 * flag is advisory, so an unrecognized value falls back rather than failing.
 */
function getBool(get: Lookup, key: string, def: boolean): boolean {
  const v = get(key)?.trim().toLowerCase();
  if (v === undefined || v === '') {
    return def;
  }
  if (v === '1' || v === 'true' || v === 'yes' || v === 'on') {
    return true;
  }
  if (v === '0' || v === 'false' || v === 'no' || v === 'off') {
    return false;
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
export function parseDuration(s: string, name = 'CI_TIMEOUT'): number {
  const text = s.trim();
  if (text === '') {
    throw new Error(`${name}: empty duration`);
  }
  // Repeated units (e.g. "90m90m") are summed per the duration-string contract — intentional,
  // not a bug.
  const re = /(\d+(?:\.\d+)?)(ns|us|µs|ms|s|m|h)/g;
  const matches = [...text.matchAll(re)];
  const consumed = matches.map((m) => m[1]! + m[2]!).join('');
  if (matches.length === 0 || consumed !== text) {
    throw new Error(`${name}: invalid duration ${JSON.stringify(s)}`);
  }
  return matches.reduce((acc, m) => acc + Number.parseFloat(m[1]!) * DURATION_UNITS_MS[m[2]!]!, 0);
}
