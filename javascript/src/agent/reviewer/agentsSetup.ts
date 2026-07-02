/**
 * The build-agent split: pure ADK wiring (category + glue LLM agents, the prompt loader, the JSON
 * generate-content config). Logic lives in the sibling modules.
 *
 * The diff is baked into each agent's system instruction because it is per-event.
 */

import { dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import { LlmAgent } from '@google/adk';

import { jsonConfig } from '../setup/generate';
import { Prompts } from '../setup/prompts';
import { type Category, Tier } from './categories';
import type { Finding } from './findings';
import {
  buildGlueInstruction,
  buildReviewInstruction,
  findingsKey,
  modelForTier,
} from './review';
import type { Engine } from './reviewer';

const prompts = new Prompts(dirname(fileURLToPath(import.meta.url)));

/**
 * Build one category review agent: an LLM agent on the category's tier whose instruction is the
 * category prompt + the filtered diff, writing its findings JSON to the category's state key.
 */
export function buildCategoryAgent(engine: Engine, c: Category, diff: string): LlmAgent {
  const body = prompts.get(c.promptName);
  return new LlmAgent({
    name: 'review_' + c.name,
    description: c.title + ' review',
    model: modelForTier(engine, c.tier),
    instruction: buildReviewInstruction(body, diff),
    outputKey: findingsKey(c.name),
    generateContentConfig: jsonConfig(),
  });
}

/**
 * Build the glue/synthesis agent: a code-tier LLM agent that sees the diff and the category
 * findings so far, emitting additional architectural-alignment / testability / test-coverage
 * findings (cross-lens dedup is done deterministically in code, not here).
 */
export function buildGlueAgent(engine: Engine, diff: string, prior: Finding[]): LlmAgent {
  const body = prompts.get('glue');
  return new LlmAgent({
    name: 'review_glue',
    description: 'Holistic synthesis review',
    model: modelForTier(engine, Tier.Code),
    instruction: buildGlueInstruction(body, diff, prior),
    generateContentConfig: jsonConfig(),
  });
}
