/**
 * Single-shot non-streaming text completion helper.
 *
 * Lets callers outside `setup` use a model without importing genai directly.
 */
import type { BaseLlm, LlmRequest } from '@google/adk';

import { contentText, userText } from './events';

/** Run one completion (`system` instruction + `user` prompt) and return its text. */
export async function generateText(llm: BaseLlm, system: string, user: string): Promise<string> {
  const req: LlmRequest = {
    contents: [userText(user)],
    config: { systemInstruction: system },
    liveConnectConfig: {},
    toolsDict: {},
  };
  const parts: string[] = [];
  for await (const resp of llm.generateContentAsync(req, false)) {
    if (resp.content) {
      parts.push(contentText(resp.content));
    }
  }
  return parts.join('');
}
