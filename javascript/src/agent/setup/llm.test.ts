// Tests for the LLM provider switch.
import { describe, expect, it } from 'vitest';

import { loadFrom } from '../../config/config';
import { buildCodeLLM, buildLLM } from './llm';
import { OllamaLlm } from './ollama';

function cfg(env: Record<string, string>) {
  return loadFrom((k) => env[k]);
}

describe('llm switch', () => {
  it('builds Ollama models for the default and code tiers', () => {
    const c = cfg({ OLLAMA_MODEL: 'gemma4:12b', OLLAMA_CODE_MODEL: 'gemma4:26b' });
    const base = buildLLM(c);
    const code = buildCodeLLM(c);
    expect(base).toBeInstanceOf(OllamaLlm);
    expect(base.model).toBe('gemma4:12b');
    expect(code.model).toBe('gemma4:26b');
  });

  it('requires a model name for Gemini', () => {
    const c = cfg({ LLM_PROVIDER: 'gemini' }); // GEMINI_MODEL empty
    expect(() => buildLLM(c)).toThrow();
  });

  it('builds a Gemini model when configured', () => {
    // The Gemini model requires credentials at construction time; gemini mode is only
    // ever selected when creds are present in the environment.
    const prev = process.env.GEMINI_API_KEY;
    process.env.GEMINI_API_KEY = 'test-key';
    try {
      const c = cfg({ LLM_PROVIDER: 'gemini', GEMINI_MODEL: 'gemini-flash-latest' });
      const base = buildLLM(c);
      expect(base.model).toBe('gemini-flash-latest');
    } finally {
      if (prev === undefined) delete process.env.GEMINI_API_KEY;
      else process.env.GEMINI_API_KEY = prev;
    }
  });
});
