/**
 * A single tool-using agent that navigates the checkout to ground a plan.
 *
 * The model decides what to read (via read_file/list_dir); no code pre-selects files.
 * Workflows use this to ground a plan (e.g. where tests belong) in the repo's actual
 * conventions rather than a hardcoded rule.
 */
import { type BaseLlm, LlmAgent } from '@google/adk';

import { driveText, newRunner } from '../setup/runner';
import { repoTools } from './tools';

/** Run a tool-using agent rooted at `repoDir` and return its final text answer. */
export async function explore(
  llm: BaseLlm,
  repoDir: string,
  instruction: string,
  input: string,
): Promise<string> {
  const agent = new LlmAgent({
    name: 'explorer',
    description: 'Examines the repository to ground a plan in its real conventions.',
    model: llm,
    instruction,
    tools: repoTools(repoDir),
  });
  const runner = newRunner('fix-explore', agent);
  return driveText(runner, 'system', 'explore', input);
}
