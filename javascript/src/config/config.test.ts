// Tests for the config loader.
import { describe, expect, it } from 'vitest';
import { loadFrom, NotifyProvider, Provider } from './config';

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
