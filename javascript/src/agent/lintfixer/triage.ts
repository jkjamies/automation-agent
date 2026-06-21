/**
 * Triage: normalize an arbitrary linter report into per-file work (LLM-backed).
 * Keeps the lint-fixer agnostic to the reporting format.
 */
import type { BaseLlm } from '@google/adk';

import { type FileWork, extractJsonArray } from '../fixflow/index';
import { generateText } from '../setup/generate';
import { prompts } from './loader';

/** Use the LLM to normalize a linter report into per-file work. */
export async function triage(llm: BaseLlm, report: string): Promise<FileWork[]> {
  const out = await generateText(llm, prompts.mustGet('triage'), report);
  const work = parseTriage(out);
  if (work.length === 0) {
    throw new Error('triage: no actionable files found in report');
  }
  return work;
}

export function parseTriage(out: string): FileWork[] {
  const js = extractJsonArray(out);
  if (js === '') {
    throw new Error('triage: no JSON array in model output');
  }
  let files: unknown;
  try {
    files = JSON.parse(js);
  } catch (err) {
    throw new Error(`triage: decode triage JSON: ${(err as Error).message}`);
  }
  const work: FileWork[] = [];
  if (!Array.isArray(files)) {
    return work;
  }
  for (const entry of files) {
    if (entry === null || typeof entry !== 'object') {
      continue;
    }
    const f = entry as Record<string, unknown>;
    const path = typeof f.path === 'string' ? f.path : '';
    if (path.trim() === '') {
      continue;
    }
    const problems = Array.isArray(f.problems) ? f.problems.map(String) : [];
    work.push({ path, items: problems });
  }
  return work;
}
