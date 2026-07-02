/**
 * The build-agent split: pure ADK wiring (category + glue + distiller LLM agents, the prompt loader,
 * the JSON generate-content config). Logic lives in the sibling modules.
 *
 * The diff / standards are baked into each agent's system instruction because they are per-event;
 * the category and glue agents get the lazy `get_rule` tool when standards are present.
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
import { type Standards, buildDistillerInstruction, standardsTools } from './standards';

const prompts = new Prompts(dirname(fileURLToPath(import.meta.url)));

/**
 * Build one category review agent: an LLM agent on the category's tier whose instruction is the
 * category prompt + the repo's standards rule menu (when any) + the filtered diff, writing its
 * findings JSON to the category's state key. When standards are present it also gets the lazy
 * get_rule tool.
 */
export function buildCategoryAgent(
  engine: Engine,
  c: Category,
  diff: string,
  std: Standards | null,
): LlmAgent {
  const body = prompts.get(c.promptName);
  return new LlmAgent({
    name: 'review_' + c.name,
    description: c.title + ' review',
    model: modelForTier(engine, c.tier),
    instruction: buildReviewInstruction(body, diff, std),
    tools: standardsTools(std),
    outputKey: findingsKey(c.name),
    generateContentConfig: jsonConfig(),
  });
}

/**
 * Build the glue/synthesis agent: a code-tier LLM agent that sees the diff, the category findings so
 * far, and the repo's standards rule menu, emitting additional architectural-alignment /
 * testability / test-coverage findings (cross-lens dedup is done deterministically in code, not
 * here). When standards are present it also gets the lazy get_rule tool.
 */
export function buildGlueAgent(
  engine: Engine,
  diff: string,
  prior: Finding[],
  std: Standards | null,
): LlmAgent {
  const body = prompts.get('glue');
  return new LlmAgent({
    name: 'review_glue',
    description: 'Holistic synthesis review',
    model: modelForTier(engine, Tier.Code),
    instruction: buildGlueInstruction(body, diff, prior, std),
    tools: standardsTools(std),
    generateContentConfig: jsonConfig(),
  });
}

/**
 * Build the standards distiller: a base-tier LLM agent (distillation is summarization/extraction,
 * the base tier) fed the reviewed repo's standards docs, prompted to emit a uniform tagged rule
 * list. It normalizes heterogeneous formats into one list.
 */
export function buildDistillerAgent(
  engine: Engine,
  docs: Map<string, string>,
  sources: string[],
): LlmAgent {
  const body = prompts.get('distill');
  return new LlmAgent({
    name: 'standards_distiller',
    description: "Distill the repo's standards docs into a tagged rule list",
    model: modelForTier(engine, Tier.Base),
    instruction: buildDistillerInstruction(body, docs, sources),
    generateContentConfig: jsonConfig(),
  });
}
