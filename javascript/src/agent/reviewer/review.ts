/**
 * The model-calling review stage: the category fan-out, the glue drive, diff formatting, and the
 * per-agent instruction composition.
 *
 * Returns the scorecard and the gated findings for the publish stage; posts nothing itself.
 */

import { type BaseAgent, type BaseLlm, ParallelAgent } from '@google/adk';

import type { PRFile } from '../../githubapi/client';
import { driveCollectState, driveText, newRunner } from '../setup/runner';
import { type Category, Tier, selectCategories } from './categories';
import { type Finding, findingsJson, parseFindings } from './findings';
import { dedupe, demoteToNitpick, dropLowConfidence } from './glue';
import type { Engine } from './reviewer';
import { type Scorecard, scoreFindings } from './scorecard';
import { type Standards, gateCitations, isEmpty } from './standards';

// The user inputs that start each drive. The real instruction (lens prompt + diff) lives in the
// agents' system instruction; these just kick generation.
export const REVIEW_TRIGGER = 'Review the diff and report findings as the JSON array specified.';
export const GLUE_TRIGGER = 'Synthesize the holistic findings as the JSON array specified.';

/**
 * Run the model-calling stage for a reviewable PR: fan out the category lenses, run the holistic
 * glue pass, then apply the deterministic verify gate (confidence drop + dedup) and score. Returns
 * the scorecard and the gated findings (the caller publishes them).
 */
export async function runReview(
  engine: Engine,
  files: PRFile[],
  std: Standards | null,
): Promise<{ card: Scorecard; findings: Finding[] }> {
  const diff = formatDiff(files);
  const cats = selectCategories(files);

  const category = await runCategoryReview(engine, diff, cats, std);
  // Glue sees the category findings as "already reported" and skips re-flagging them, so it must
  // see only the findings that survive the same gates as the final output. Otherwise a finding the
  // verify/citation gate later drops is suppressed in glue and then dropped here, vanishing from the
  // review entirely.
  const gatedForGlue = gateCitations(
    engine,
    dropLowConfidence([...category], engine.minConfidence),
    std,
  );
  const glue = await runGlue(engine, diff, gatedForGlue, std);

  let all = [...category, ...glue];
  all = dropLowConfidence(all, engine.minConfidence); // phase-1 verify gate
  all = gateCitations(engine, all, std); // citation gate
  all = dedupe(all); // cross-lens dedup
  return { card: scoreFindings(all), findings: all };
}

/**
 * Build one agent per applicable category, run them in parallel (ADK ParallelAgent — genuine
 * concurrency on Vertex, GPU-serialized locally with no code change), and return every category's
 * parsed findings. Empty findings is success. The "(other)" catch-all's findings are demoted to
 * nitpick.
 */
export async function runCategoryReview(
  engine: Engine,
  diff: string,
  cats: Category[],
  std: Standards | null,
): Promise<Finding[]> {
  // Deferred import breaks the review <-> agentsSetup module cycle.
  const agentsSetup = await import('./agentsSetup');

  const agents: BaseAgent[] = cats.map((c) => agentsSetup.buildCategoryAgent(engine, c, diff, std));
  const parallel = new ParallelAgent({
    name: 'review_all',
    description: 'Per-category review in parallel',
    subAgents: agents,
  });
  const runner = newRunner('reviewer-review', parallel);
  const state = await driveCollectState(runner, 'system', 'review', REVIEW_TRIGGER);

  const out: Finding[] = [];
  for (const c of cats) {
    const key = findingsKey(c.name);
    if (!(key in state)) {
      // A lens that ran but found nothing is normal (empty = success); a missing state key means it
      // produced no output at all. Log it, but don't fail the whole review on one lens —
      // best-effort by design.
      engine.log.warn('category produced no findings output', { category: c.name });
    }
    const raw = state[key];
    let found = parseFindings(typeof raw === 'string' ? raw : '');
    if (c.other) {
      found = demoteToNitpick(found);
    }
    out.push(...found);
  }
  return out;
}

/**
 * Run the holistic synthesis pass over the diff and the category findings, returning the additional
 * architectural/testability/coverage findings it produced. Empty is success.
 */
export async function runGlue(
  engine: Engine,
  diff: string,
  prior: Finding[],
  std: Standards | null,
): Promise<Finding[]> {
  const agentsSetup = await import('./agentsSetup');

  const agent = agentsSetup.buildGlueAgent(engine, diff, prior, std);
  const runner = newRunner('reviewer-glue', agent);
  const text = await driveText(runner, 'system', 'glue', GLUE_TRIGGER);
  return parseFindings(text);
}

/**
 * Render the filtered files as one prompt-ready diff: a header per file plus its patch in a fenced
 * block. A file with no patch (binary/oversized) is noted so the model knows it changed without a
 * hunk to review.
 */
export function formatDiff(files: PRFile[]): string {
  const parts: string[] = [];
  for (const f of files) {
    if (f.status === 'renamed' && f.previousPath !== '') {
      parts.push(`### ${f.path} (renamed from ${f.previousPath})\n`);
    } else {
      parts.push(`### ${f.path} (${f.status})\n`);
    }
    if (f.patch.trim() === '') {
      parts.push('(no textual diff available)\n\n');
      continue;
    }
    // Patch content is untrusted (it can be a diff of a Markdown/RST file that itself contains ```
    // runs), so pick a fence longer than the longest backtick run in the patch — otherwise an
    // embedded run would close the block early and corrupt the prompt structure.
    let fence = '`'.repeat(maxBacktickRun(f.patch) + 1);
    if (fence.length < 3) {
      fence = '```';
    }
    parts.push(fence + 'diff\n');
    parts.push(f.patch);
    if (!f.patch.endsWith('\n')) {
      parts.push('\n');
    }
    parts.push(fence + '\n\n');
  }
  return parts.join('');
}

/**
 * Return the length of the longest run of consecutive backticks in `s` (0 if none), used to size a
 * fence that the content cannot break out of.
 */
export function maxBacktickRun(s: string): number {
  let longest = 0;
  let cur = 0;
  for (const ch of s) {
    if (ch === '`') {
      cur++;
      if (cur > longest) {
        longest = cur;
      }
    } else {
      cur = 0;
    }
  }
  return longest;
}

/** The session-state key a category agent writes its findings JSON to. */
export function findingsKey(name: string): string {
  return 'findings:' + name;
}

/**
 * Return the LLM a category runs on (code tier → code model, else base model). Reached only from
 * the review path, which kickoff guards behind a non-null model check, so the tier model is present.
 */
export function modelForTier(engine: Engine, tier: Tier): BaseLlm {
  const model = tier === Tier.Code ? engine.codeLlm : engine.baseLlm;
  if (model === null) {
    throw new Error('reviewer: review model not configured');
  }
  return model;
}

/**
 * Compose a category agent's instruction: the lens prompt, the repo's standards rule menu (when
 * any), and the filtered diff (baked in because they are per-event).
 */
export function buildReviewInstruction(promptBody: string, diff: string, std: Standards | null): string {
  const parts = [promptBody];
  writeStandardsMenu(parts, std);
  parts.push('\n\n## Diff under review\n\n');
  parts.push(diff);
  return parts.join('');
}

/**
 * Compose the glue agent's instruction: the glue prompt, the standards menu, the diff, and the
 * findings the category agents already produced (so it reasons holistically without re-flagging
 * them).
 */
export function buildGlueInstruction(
  promptBody: string,
  diff: string,
  prior: Finding[],
  std: Standards | null,
): string {
  const parts = [promptBody];
  writeStandardsMenu(parts, std);
  parts.push('\n\n## Diff under review\n\n');
  parts.push(diff);
  parts.push('\n\n## Findings already reported by other lenses\n\n');
  parts.push(findingsJson(prior));
  return parts.join('');
}

/**
 * Append the repo's compact rule menu and the citation instruction to an agent prompt when
 * standards were discovered. The full text of any rule is available via getRule.
 */
export function writeStandardsMenu(parts: string[], std: Standards | null): void {
  if (isEmpty(std)) {
    return;
  }
  const real = std!;
  parts.push('\n\n## Repo standards (cite rule_id for conformance findings)\n\n');
  parts.push(real.menu());
  parts.push(
    '\nWhen a finding is a violation of one of these rules, set its dimension to the ' +
      'rule\'s dimension and set "rule_id" to the rule\'s id. Call get_rule(id) to read a ' +
      "rule's full text before flagging. Never invent a rule id; a pattern/architecture " +
      'finding with no matching rule is not a standards violation.\n',
  );
}
