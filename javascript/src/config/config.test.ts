// Tests for the config loader.
import { generateKeyPairSync } from 'node:crypto';
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { inspect } from 'node:util';
import { afterAll, describe, expect, it } from 'vitest';
import { appMode, loadFrom, NotifyProvider, Provider, SessionBackend, TasksBackend } from './config';

function mapLookup(m: Record<string, string>) {
  return (key: string): string | undefined => m[key];
}

/** A complete, valid cloudtasks configuration (the base for negative cases). */
function fullCloudtasksEnv(): Record<string, string> {
  return {
    TASKS_BACKEND: 'cloudtasks',
    TASKS_PROJECT: 'proj',
    TASKS_LOCATION: 'us-central1',
    TASKS_QUEUE: 'agent-q',
    DISPATCH_URL: 'https://svc.run.app/internal/dispatch',
    INTERNAL_TOKEN: 'sekret',
    GITHUB_WEBHOOK_SECRET: 'hmac',
  };
}

describe('config', () => {
  it('loads defaults', () => {
    const c = loadFrom(mapLookup({}));
    expect(c.llmProvider).toBe(Provider.Ollama);
    expect(c.ollamaModel).toBe('gemma4:12b');
    expect(c.ollamaCodeModel).toBe('gemma4:26b'); // default
    expect(c.notifyProvider).toBe(NotifyProvider.Slack);
    expect(c.maxIterations).toBe(3);
    expect(c.ciTimeoutMs).toBe(90 * 60 * 1000);
    expect(c.sessionBackend).toBe(SessionBackend.Memory);
    expect(c.sqliteDsn).toBe('automation-agent.db');
    expect(c.firestoreProject).toBe('');
    expect(c.firestoreCollection).toBe('automation_agent');
    expect(c.internalToken).toBe('');
    expect(c.agentPrLabel).toBe('automation-agent'); // default
  });

  it('reads a custom agent PR label', () => {
    const c = loadFrom(mapLookup({ AGENT_PR_LABEL: 'my-bot' }));
    expect(c.agentPrLabel).toBe('my-bot');
  });

  it('reads the session backend and its settings', () => {
    const c = loadFrom(
      mapLookup({
        SESSION_BACKEND: 'firestore',
        SQLITE_DSN: 'runs.db',
        FIRESTORE_PROJECT: 'my-proj',
        FIRESTORE_COLLECTION: 'agent_runs',
        INTERNAL_TOKEN: 'secret',
      }),
    );
    expect(c.sessionBackend).toBe(SessionBackend.Firestore);
    expect(c.sqliteDsn).toBe('runs.db');
    expect(c.firestoreProject).toBe('my-proj');
    expect(c.firestoreCollection).toBe('agent_runs');
    expect(c.internalToken).toBe('secret');
  });

  it('rejects an invalid session backend', () => {
    expect(() => loadFrom(mapLookup({ SESSION_BACKEND: 'redis' }))).toThrow();
  });

  it('parses the repos list', () => {
    const c = loadFrom(mapLookup({ REPOS: ' a/b , c/d ,, e/f ' }));
    expect(c.repos).toEqual(['a/b', 'c/d', 'e/f']);
  });

  it('honours the code-model override', () => {
    const c = loadFrom(mapLookup({ OLLAMA_MODEL: 'gemma4:12b', OLLAMA_CODE_MODEL: 'gemma4:26b' }));
    expect(c.ollamaCodeModel).toBe('gemma4:26b');
    expect(c.ollamaModel).toBe('gemma4:12b');
  });

  it('falls back to GH_TOKEN, with GITHUB_TOKEN taking precedence', () => {
    // GH_TOKEN is honoured when GITHUB_TOKEN is unset, so a local gh-style env works.
    expect(loadFrom(mapLookup({ GH_TOKEN: 'gh_abc' })).githubToken).toBe('gh_abc');
    // GITHUB_TOKEN wins over GH_TOKEN.
    expect(loadFrom(mapLookup({ GITHUB_TOKEN: 'primary', GH_TOKEN: 'fallback' })).githubToken).toBe(
      'primary',
    );
  });

  it('defaults the git transport to https', () => {
    const c = loadFrom(mapLookup({}));
    expect(c.gitTransport).toBe('https');
    expect(c.gitSshKey).toBe('');
  });

  it('reads the ssh git transport and an explicit key', () => {
    const c = loadFrom(mapLookup({ GIT_TRANSPORT: 'ssh', GIT_SSH_KEY: '/home/dev/.ssh/id_ed25519' }));
    expect(c.gitTransport).toBe('ssh');
    expect(c.gitSshKey).toBe('/home/dev/.ssh/id_ed25519');
  });

  it('rejects an invalid git transport', () => {
    expect(() => loadFrom(mapLookup({ GIT_TRANSPORT: 'rsync' }))).toThrow();
  });

  it('rejects an invalid LLM provider', () => {
    expect(() => loadFrom(mapLookup({ LLM_PROVIDER: 'openai' }))).toThrow();
  });

  it('rejects an invalid notify provider', () => {
    expect(() => loadFrom(mapLookup({ NOTIFY_PROVIDER: 'discord' }))).toThrow();
  });

  it('rejects an invalid duration', () => {
    expect(() => loadFrom(mapLookup({ CI_TIMEOUT: 'soon' }))).toThrow();
  });

  it('enforces the max-iterations floor', () => {
    expect(() => loadFrom(mapLookup({ MAX_ITERATIONS: '0' }))).toThrow();
  });

  it('rejects an unparseable max-iterations', () => {
    expect(() => loadFrom(mapLookup({ MAX_ITERATIONS: 'lots' }))).toThrow();
  });

  it('parses a compound duration', () => {
    const c = loadFrom(mapLookup({ CI_TIMEOUT: '1h30m' }));
    expect(c.ciTimeoutMs).toBe(90 * 60 * 1000);
  });

  it('rejects a non-numeric PORT', () => {
    expect(() => loadFrom(mapLookup({ PORT: 'abc' }))).toThrow();
  });

  it('rejects a PORT out of range', () => {
    expect(() => loadFrom(mapLookup({ PORT: '70000' }))).toThrow();
  });
});

// --- GitHub App mode ---------------------------------------------------------
// Mirrors python/tests/test_config_app.py and go/internal/config/config_app_test.go: the
// env-var contract, App-vs-PAT mode selection, positive-id and RSA-PEM validation, the
// flattened-`\n` unescape (including the trailing-newline regression), and the empty-REPOS
// rejection. No network — throwaway keys generated in-process.

function rsaPem(pkcs1 = false): string {
  const { privateKey } = generateKeyPairSync('rsa', {
    modulusLength: 2048,
    publicKeyEncoding: { type: 'spki', format: 'pem' },
    privateKeyEncoding: { type: pkcs1 ? 'pkcs1' : 'pkcs8', format: 'pem' },
  });
  return privateKey;
}

function ecPem(): string {
  const { privateKey } = generateKeyPairSync('ec', {
    namedCurve: 'P-256',
    publicKeyEncoding: { type: 'spki', format: 'pem' },
    privateKeyEncoding: { type: 'pkcs8', format: 'pem' },
  });
  return privateKey;
}

// A valid RSA key shared across cases that don't probe key parsing (keygen is slow).
const APP_PEM = rsaPem();

function appEnv(overrides: Record<string, string>): Record<string, string> {
  // The full set of vars that select App mode; REPOS is included because App mode rejects
  // an empty allow-list.
  return {
    GITHUB_APP_ID: '42',
    GITHUB_APP_INSTALLATION_ID: '99',
    GITHUB_APP_PRIVATE_KEY: APP_PEM,
    REPOS: 'acme/api',
    ...overrides,
  };
}

describe('config: github app', () => {
  const tmpRoot = mkdtempSync(join(tmpdir(), 'config-app-'));
  afterAll(() => rmSync(tmpRoot, { recursive: true, force: true }));

  let keyFileSeq = 0;
  function writeKeyFile(contents: string): string {
    const p = join(tmpRoot, `key-${keyFileSeq++}.pem`);
    writeFileSync(p, contents);
    return p;
  }

  it('stays in PAT mode when no App vars are set', () => {
    const c = loadFrom(mapLookup({ GITHUB_TOKEN: 'pat', REPOS: 'acme/api' }));
    expect(appMode(c)).toBe(false);
    expect(c.githubAppId).toBe(0);
    expect(c.githubAppInstallationId).toBe(0);
    expect(c.githubAppPrivateKeyPem).toBe('');
  });

  it('selects App mode from the full var set', () => {
    const c = loadFrom(mapLookup(appEnv({})));
    expect(appMode(c)).toBe(true);
    expect(c.githubAppId).toBe(42);
    expect(c.githubAppInstallationId).toBe(99);
    expect(c.githubAppPrivateKeyPem).toContain('-----BEGIN');
  });

  it('accepts a PKCS#1 RSA key', () => {
    const c = loadFrom(mapLookup(appEnv({ GITHUB_APP_PRIVATE_KEY: rsaPem(true) })));
    expect(appMode(c)).toBe(true);
  });

  it('reads the key from a file', () => {
    const keyPath = writeKeyFile(APP_PEM);
    const c = loadFrom(
      mapLookup(appEnv({ GITHUB_APP_PRIVATE_KEY: '', GITHUB_APP_PRIVATE_KEY_PATH: keyPath })),
    );
    expect(appMode(c)).toBe(true);
    expect(c.githubAppPrivateKeyPem).toContain('-----BEGIN');
  });

  it('unescapes a flattened (literal \\n) key', () => {
    const flattened = APP_PEM.replaceAll('\n', '\\n');
    const c = loadFrom(mapLookup(appEnv({ GITHUB_APP_PRIVATE_KEY: flattened })));
    expect(appMode(c)).toBe(true);
    expect(c.githubAppPrivateKeyPem).not.toContain('\\n'); // escaped \n restored
  });

  it('unescapes a flattened key that also has a real trailing newline (from a file)', () => {
    // A secret store can flatten newlines to literal `\n` and still append one real trailing
    // newline; the unescape must run on the escaped sequences regardless. The file path is
    // read untrimmed, so this exercises the corrected condition directly.
    const keyPath = writeKeyFile(APP_PEM.replaceAll('\n', '\\n') + '\n');
    const c = loadFrom(
      mapLookup(appEnv({ GITHUB_APP_PRIVATE_KEY: '', GITHUB_APP_PRIVATE_KEY_PATH: keyPath })),
    );
    expect(appMode(c)).toBe(true);
    expect(c.githubAppPrivateKeyPem).not.toContain('\\n');
  });

  it.each([
    ['missing app id', { GITHUB_APP_ID: '' }],
    ['missing installation', { GITHUB_APP_INSTALLATION_ID: '' }],
    ['missing key', { GITHUB_APP_PRIVATE_KEY: '' }],
    ['both key sources', { GITHUB_APP_PRIVATE_KEY_PATH: '/some/key.pem' }],
    ['zero app id', { GITHUB_APP_ID: '0' }],
    ['negative app id', { GITHUB_APP_ID: '-1' }],
    ['non-numeric app id', { GITHUB_APP_ID: 'abc' }],
    ['zero installation', { GITHUB_APP_INSTALLATION_ID: '0' }],
    ['non-numeric installation', { GITHUB_APP_INSTALLATION_ID: 'x' }],
    ['invalid pem', { GITHUB_APP_PRIVATE_KEY: 'not a pem' }],
    ['empty repos in app mode', { REPOS: '' }],
  ])('rejects %s', (_name, overrides) => {
    expect(() => loadFrom(mapLookup(appEnv(overrides)))).toThrow();
  });

  it('rejects a non-RSA (EC) key', () => {
    expect(() => loadFrom(mapLookup(appEnv({ GITHUB_APP_PRIVATE_KEY: ecPem() })))).toThrow(/RSA/);
  });

  it('rejects an unreadable key file', () => {
    expect(() =>
      loadFrom(
        mapLookup(
          appEnv({ GITHUB_APP_PRIVATE_KEY: '', GITHUB_APP_PRIVATE_KEY_PATH: '/no/such/key.pem' }),
        ),
      ),
    ).toThrow(/read GITHUB_APP_PRIVATE_KEY_PATH/);
  });

  it('masks every credential when inspected/logged', () => {
    const c = loadFrom(
      mapLookup(
        appEnv({
          GITHUB_TOKEN: 'ghp_supersecretpat',
          GITHUB_WEBHOOK_SECRET: 'webhook-shhh',
          INTERNAL_TOKEN: 'internal-shhh',
          SLACK_WEBHOOK_URL: 'https://hooks.slack.com/services/SECRETPATH',
        }),
      ),
    );
    const rendered = inspect(c);
    for (const leak of [
      'ghp_supersecretpat',
      'webhook-shhh',
      'internal-shhh',
      'SECRETPATH',
      'PRIVATE KEY',
    ]) {
      expect(rendered).not.toContain(leak);
    }
    expect(rendered).toContain('***');
    // Redaction is display-only: the real values stay readable on the object itself.
    expect(c.githubToken).toBe('ghp_supersecretpat');
    expect(c.githubAppPrivateKeyPem).toContain('PRIVATE KEY');
  });
});

describe('config: execution transport (TASKS_BACKEND)', () => {
  it('defaults to the in-process backend with a 30m deadline', () => {
    const c = loadFrom(mapLookup({}));
    expect(c.tasksBackend).toBe(TasksBackend.InProcess);
    expect(c.tasksDispatchDeadlineMs).toBe(30 * 60 * 1000);
  });

  it('rejects an unknown backend', () => {
    expect(() => loadFrom(mapLookup({ TASKS_BACKEND: 'kafka' }))).toThrow();
  });

  it('requires the full cloudtasks configuration', () => {
    // Missing everything but the backend selector.
    expect(() => loadFrom(mapLookup({ TASKS_BACKEND: 'cloudtasks' }))).toThrow();
    // Drop one required key at a time from an otherwise-valid config.
    for (const key of ['TASKS_LOCATION', 'TASKS_QUEUE', 'DISPATCH_URL', 'INTERNAL_TOKEN', 'GITHUB_WEBHOOK_SECRET']) {
      const env = fullCloudtasksEnv();
      delete env[key];
      expect(() => loadFrom(mapLookup(env)), `missing ${key}`).toThrow();
    }
  });

  it('rejects an insecure or mis-pathed DISPATCH_URL', () => {
    // DISPATCH_URL must be an absolute https URL ending in /internal/dispatch — the task carries
    // the Bearer token to it, so a plaintext / wrong-path target is rejected.
    for (const bad of [
      'http://svc.run.app/internal/dispatch', // plaintext
      '/internal/dispatch', // relative
      'not a url', // garbage
      'https://svc.run.app/', // right scheme, wrong path
    ]) {
      const env = fullCloudtasksEnv();
      env.DISPATCH_URL = bad;
      expect(() => loadFrom(mapLookup(env)), bad).toThrow();
    }
  });

  it('rejects an out-of-range dispatch deadline', () => {
    // Cloud Tasks clamps an HTTP-target dispatch deadline to 15s..30m.
    for (const bad of ['10s', '45m']) {
      const env = fullCloudtasksEnv();
      env.TASKS_DISPATCH_DEADLINE = bad;
      expect(() => loadFrom(mapLookup(env)), bad).toThrow();
    }
  });

  it('loads a full cloudtasks configuration', () => {
    const c = loadFrom(mapLookup(fullCloudtasksEnv()));
    expect(c.tasksBackend).toBe(TasksBackend.CloudTasks);
    expect(c.tasksProject).toBe('proj');
    expect(c.tasksLocation).toBe('us-central1');
    expect(c.tasksQueue).toBe('agent-q');
    expect(c.dispatchUrl).toBe('https://svc.run.app/internal/dispatch');
  });

  it('falls back to GOOGLE_CLOUD_PROJECT for the tasks project', () => {
    const env = fullCloudtasksEnv();
    delete env.TASKS_PROJECT;
    env.GOOGLE_CLOUD_PROJECT = 'ambient';
    const c = loadFrom(mapLookup(env));
    expect(c.tasksProject).toBe('ambient');
  });
});
