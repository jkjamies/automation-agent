// Tests for the config loader.
import { describe, expect, it } from 'vitest';
import { loadFrom, NotifyProvider, Provider, SessionBackend } from './config';

function mapLookup(m: Record<string, string>) {
  return (key: string): string | undefined => m[key];
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
