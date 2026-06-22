/**
 * Analyze: rewrite each affected source file to fix its lint problems.
 *
 * One parallel agent per file, reading the current source from the checkout. Feedback
 * (on retry) is the previous attempt's CI failure.
 */
import {
  type AnalyzeInput,
  type FileEdit,
  type FileWork,
  coder,
  parallelAnalyze,
  readFile,
  stripFences,
} from '../fixflow/index';
import { generateText } from '../setup/generate';
import { prompts } from './loader';

/** Rewrite each affected source file to fix its lint problems, in parallel. */
export async function analyze(input: AnalyzeInput): Promise<FileEdit[]> {
  const edit = async (w: FileWork): Promise<FileEdit> => {
    let src: string;
    try {
      src = readFile(input.repoDir, w.path);
    } catch (err) {
      // Any read error (including a path that escapes the repo root) -> skip. Log it so a
      // skip is distinguishable from "nothing to do".
      input.log?.warn('lint analyze: skipping unreadable file', { path: w.path, err: String(err) });
      return { path: '', content: '' };
    }
    const out = await generateText(
      coder(input),
      prompts.mustGet('analyze'),
      buildFilePrompt(w, src, input.feedback),
    );
    return { path: w.path, content: stripFences(out) };
  };

  return parallelAnalyze(input.work, edit);
}

export function buildFilePrompt(w: FileWork, content: string, ciFeedback: string): string {
  const lines = [`File: ${w.path}\n`, 'Lint problems to fix:'];
  for (const p of w.items) {
    lines.push(`- ${p}`);
  }
  let body = lines.join('\n');
  if (ciFeedback !== '') {
    body += '\n\nThe previous attempt failed CI with:\n' + ciFeedback;
  }
  body +=
    '\n\nCurrent file content:\n```\n' +
    content +
    '\n```\n\nOutput ONLY the complete corrected content of this file — ' +
    'no explanation, no markdown fences.';
  return body;
}
