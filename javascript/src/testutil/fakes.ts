/**
 * Shared test fakes.
 *
 * `FakeLlm` is a `BaseLlm` that yields scripted text and records the requests it
 * received, so we can test agent wiring and deterministic logic without a real
 * model. We never assert on real LLM output.
 *
 * This directory is test-only: excluded from coverage and from the arch
 * import-boundary checks.
 */
import { BaseLlm, type LlmRequest, type LlmResponse } from '@google/adk';
import type { Content } from '@google/genai';

import { contentText } from '../agent/setup/events';

/** A deterministic BaseLlm that yields fixed text responses in order. */
export class FakeLlm extends BaseLlm {
  private readonly texts: string[];
  private idx = 0;
  readonly requests: LlmRequest[] = [];

  constructor(...texts: string[]) {
    super({ model: 'fake' });
    this.texts = texts.length > 0 ? texts : [''];
  }

  override async *generateContentAsync(req: LlmRequest): AsyncGenerator<LlmResponse, void> {
    this.requests.push(req);
    const text = this.texts[Math.min(this.idx, this.texts.length - 1)]!;
    this.idx += 1;
    yield {
      content: { role: 'model', parts: [{ text }] },
      turnComplete: true,
    };
  }

  override async connect(): Promise<never> {
    throw new Error('FakeLlm does not support live connections');
  }
}

/**
 * A BaseLlm that routes by system instruction (triage / explore-plan / execute),
 * yielding the matching scripted text. Used by covfixer/fixflow tests to drive the
 * multi-phase loop deterministically; tests assert on structure, never LLM content.
 */
export class ScriptedLlm extends BaseLlm {
  constructor(
    private readonly scripts: { triage?: string; plan?: string; test?: string } = {},
  ) {
    super({ model: 'scripted' });
  }

  override async *generateContentAsync(req: LlmRequest): AsyncGenerator<LlmResponse, void> {
    const si = req.config?.systemInstruction;
    const sys = typeof si === 'string' ? si : contentText(si as Content);
    let resp = this.scripts.test ?? '';
    if (sys.includes('triaging')) {
      resp = this.scripts.triage ?? '';
    } else if (sys.includes('planning where to add')) {
      resp = this.scripts.plan ?? '';
    }
    yield { content: { role: 'model', parts: [{ text: resp }] }, turnComplete: true };
  }

  override async connect(): Promise<never> {
    throw new Error('ScriptedLlm does not support live connections');
  }
}
