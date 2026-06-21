/**
 * Triage: normalize an arbitrary coverage report into source files with meaningful
 * uncovered logic (LLM-backed).
 */
import type { BaseLlm } from '@google/adk';

import { type FileWork, extractJsonArray } from '../fixflow/index';
import { generateText } from '../setup/generate';
import { prompts } from './loader';

/** Use the LLM to find the source files with meaningful uncovered logic. */
export async function triage(llm: BaseLlm, report: string): Promise<FileWork[]> {
  const out = await generateText(llm, prompts.mustGet('triage'), report);
  const work = parseTriage(out);
  if (work.length === 0) {
    throw new Error('triage: no meaningful uncovered files found in report');
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
  if (Array.isArray(files)) {
    for (const f of files) {
      const path = f && typeof f === 'object' && typeof f.path === 'string' ? f.path : '';
      if (path.trim() !== '') {
        const uncovered = Array.isArray(f.uncovered) ? f.uncovered.map(String) : [];
        work.push({ path, items: uncovered });
      }
    }
  }
  return work;
}
