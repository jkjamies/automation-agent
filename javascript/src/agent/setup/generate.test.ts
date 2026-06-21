// Tests for the single-shot text generation helper.
import { describe, expect, it } from 'vitest';

import { FakeLlm } from '../../testutil/fakes';
import { contentText } from './events';
import { generateText } from './generate';

describe('generateText', () => {
  it('runs one completion and returns its text', async () => {
    const llm = new FakeLlm('the answer');
    const out = await generateText(llm, 'you are a bot', 'do the thing');
    expect(out).toBe('the answer');

    const req = llm.requests[0]!;
    expect(req.config?.systemInstruction).toBe('you are a bot');
    expect(contentText(req.contents[0])).toBe('do the thing');
  });
});
