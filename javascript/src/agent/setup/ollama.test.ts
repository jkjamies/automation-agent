// Tests for the Ollama adapter. Stubs fetch to capture the request and return an
// Ollama-shaped response.
import type { LlmRequest } from '@google/adk';
import { Type } from '@google/genai';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { OllamaLlm } from './ollama';

afterEach(() => vi.unstubAllGlobals());

function stub(responseBody: unknown, status = 200): ReturnType<typeof vi.fn> {
  const fn = vi.fn(async () => new Response(JSON.stringify(responseBody), { status }));
  vi.stubGlobal('fetch', fn);
  return fn;
}

function req(): LlmRequest {
  return {
    contents: [{ role: 'user', parts: [{ text: 'fix it' }] }],
    config: {
      systemInstruction: 'you are a fixer',
      temperature: 0.2,
      tools: [
        {
          functionDeclarations: [
            {
              name: 'apply',
              description: 'apply a fix',
              parameters: {
                type: Type.OBJECT,
                properties: { path: { type: Type.STRING } },
                required: ['path'],
              },
            },
          ],
        },
      ],
    },
    liveConnectConfig: {},
    toolsDict: {},
  };
}

async function first<T>(gen: AsyncGenerator<T>): Promise<T> {
  for await (const v of gen) return v;
  throw new Error('generator yielded nothing');
}

describe('OllamaLlm', () => {
  it('rejects an empty model tag', () => {
    expect(() => new OllamaLlm('http://localhost:11434', '')).toThrow();
  });

  it('forwards messages, options and tools, and parses tool calls', async () => {
    const fn = stub({
      model: 'gemma4:12b',
      message: {
        content: 'done',
        tool_calls: [{ function: { name: 'apply', arguments: { path: 'a.ts' } } }],
      },
    });
    const m = new OllamaLlm('http://localhost:11434/', 'gemma4:12b');
    const resp = await first(m.generateContentAsync(req()));

    // Request shape
    const [url, init] = fn.mock.calls[0]!;
    expect(url).toBe('http://localhost:11434/api/chat');
    const body = JSON.parse((init as RequestInit).body as string);
    expect(body.model).toBe('gemma4:12b');
    expect(body.stream).toBe(false);
    expect(body.options.num_ctx).toBe(32768);
    expect(body.options.temperature).toBe(0.2); // honoured from config
    expect(body.messages[0]).toEqual({ role: 'system', content: 'you are a fixer' });
    expect(body.messages[1]).toMatchObject({ role: 'user', content: 'fix it' });
    expect(body.tools[0].function.name).toBe('apply');
    expect(body.tools[0].function.parameters.type).toBe('object'); // lowercased
    expect(body.tools[0].function.parameters.properties.path.type).toBe('string');

    // Response parsing: text part + function-call part with id falling back to name
    const parts = resp.content!.parts!;
    expect(parts[0]).toEqual({ text: 'done' });
    expect(parts[1]!.functionCall).toEqual({ id: 'apply', name: 'apply', args: { path: 'a.ts' } });
    expect(resp.turnComplete).toBe(true);
  });

  it('throws on a non-2xx response', async () => {
    stub('boom', 500);
    const m = new OllamaLlm('http://localhost:11434', 'gemma4:12b');
    await expect(first(m.generateContentAsync(req()))).rejects.toThrow(/ollama chat/);
  });

  it('defaults temperature to 0 and sets a JSON format when requested', async () => {
    const fn = stub({ model: 'g', message: { content: '{}' } });
    const m = new OllamaLlm('http://localhost:11434', 'gemma4:12b');
    const r = req();
    r.config = { responseMimeType: 'application/json' };
    await first(m.generateContentAsync(r));
    const body = JSON.parse((fn.mock.calls[0]![1] as RequestInit).body as string);
    expect(body.options.temperature).toBe(0);
    expect(body.format).toBe('json');
  });
});
