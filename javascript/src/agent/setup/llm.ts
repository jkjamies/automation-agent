/**
 * The LLM provider switch and shared agent-building utilities.
 *
 * `setup` is the ONLY module permitted to import provider SDKs (the Ollama adapter,
 * Gemini) — enforced by the arch tests. Agents depend only on the returned
 * `BaseLlm`, so switching providers is a config change, not a code change. See
 * docs/architecture.md §4.
 */
import type { BaseLlm } from '@google/adk';

import { type Config, Provider } from '../../config/config';
import { newGeminiModel } from './gemini';
import { OllamaLlm } from './ollama';

/**
 * Return the default model (triage, explore, summary) for the configured provider.
 */
export function buildLLM(cfg: Config): BaseLlm {
  return build(cfg, cfg.ollamaModel, cfg.geminiModel);
}

/**
 * Return the model for the code-change steps (lint rewrite, coverage test
 * generation) — typically a larger model. Falls back to the default model when no
 * code model is configured.
 */
export function buildCodeLLM(cfg: Config): BaseLlm {
  return build(cfg, cfg.ollamaCodeModel, cfg.geminiCodeModel);
}

function build(cfg: Config, ollamaModel: string, geminiModel: string): BaseLlm {
  switch (cfg.llmProvider) {
    case Provider.Ollama:
      return new OllamaLlm(cfg.ollamaHost, ollamaModel);
    case Provider.Gemini:
      return newGeminiModel(geminiModel);
    default:
      throw new Error(`unknown LLM provider ${JSON.stringify(cfg.llmProvider)}`);
  }
}
