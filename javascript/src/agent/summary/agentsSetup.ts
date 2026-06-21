/**
 * Builds the summary workflow agent.
 *
 *     Sequential[ Parallel[fetch×N] -> summarize(LLM) -> notify ]
 *
 * Fetchers write per-repo commit data to state; the summarizer reads it via its
 * instruction provider and writes the digest; the notifier posts it.
 */
import { fileURLToPath } from 'node:url';
import { dirname } from 'node:path';
import { type BaseAgent, type BaseLlm, LlmAgent, ParallelAgent, SequentialAgent } from '@google/adk';

import type { Notifier } from '../../notify/notify';
import { Prompts } from '../setup/prompts';
import {
  type CommitLister,
  DIGEST_KEY,
  defaultNow,
  newFetchAgent,
  newNotifyAgent,
  summaryInstruction,
} from './summary';

const prompts = new Prompts(dirname(fileURLToPath(import.meta.url)));

const DEFAULT_WINDOW_MS = 24 * 60 * 60 * 1000;
const DEFAULT_TITLE = 'Daily commit digest';

/** Injected dependencies for the summary workflow. */
export interface Deps {
  llm: BaseLlm;
  gh: CommitLister;
  notify: Notifier;
  repos: string[]; // owner/repo entries; one parallel fetcher each
  windowMs?: number; // commit window; defaults to 24h
  title?: string; // digest notification title; defaults to "Daily commit digest"
  now?: () => Date; // injectable clock
}

/**
 * Wire the summary workflow.
 *
 * @throws Error if no repos are configured or a required dependency is missing.
 */
export function buildSummaryAgent(d: Deps): SequentialAgent {
  if (!d.repos || d.repos.length === 0) {
    throw new Error('summary: at least one repo is required');
  }
  if (!d.llm || !d.gh || !d.notify) {
    throw new Error('summary: llm, gh and notify are required');
  }

  const now = d.now ?? defaultNow;
  const windowMs = d.windowMs && d.windowMs > 0 ? d.windowMs : DEFAULT_WINDOW_MS;
  const title = d.title && d.title !== '' ? d.title : DEFAULT_TITLE;

  const fetchers: BaseAgent[] = d.repos.map((repo) => newFetchAgent(repo, d.gh, windowMs, now));
  const parallel = new ParallelAgent({
    name: 'fetch_all',
    description: 'Fetches recent commits for all configured repositories',
    subAgents: fetchers,
  });

  const summarizer = new LlmAgent({
    name: 'summarizer',
    description: 'Summarizes recent commits into a digest',
    model: d.llm,
    instruction: summaryInstruction(prompts.mustGet('summarize')),
    outputKey: DIGEST_KEY,
  });

  return new SequentialAgent({
    name: 'summary_workflow',
    description: 'Commit digest workflow',
    subAgents: [parallel, summarizer, newNotifyAgent(d.notify, title)],
  });
}
