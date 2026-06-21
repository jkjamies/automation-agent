/**
 * Analyze: a two-phase test-generation step (explore a plan, then execute it).
 *
 * The explore phase runs a tool-using agent that navigates the checkout itself
 * (read_file / list_dir) to learn the repo's real test conventions and returns a
 * per-file plan; the execute phase generates a test per file from that plan + the
 * source, one parallel agent per file.
 */
import {
  type AnalyzeInput,
  type FileEdit,
  type FileWork,
  coder,
  explore as fixflowExplore,
  extractJsonArray,
  parallelAnalyze,
  readFile,
  stripFences,
} from '../fixflow/index';
import { generateText } from '../setup/generate';
import { prompts } from './loader';

/**
 * The explorer's decision for one source file, grounded in the repo's actual existing
 * tests (never derived from a fixed rule).
 */
export interface PlanEntry {
  source: string;
  testPath: string;
  framework: string;
  notes: string;
}

/**
 * Plan test placement by examining the repo's real conventions, then generate a test per
 * file from that plan.
 */
export async function analyze(input: AnalyzeInput): Promise<FileEdit[]> {
  const plan = await explorePlan(input);
  return execute(input, plan);
}

async function explorePlan(input: AnalyzeInput): Promise<Map<string, PlanEntry>> {
  const out = await fixflowExplore(
    input.llm,
    input.repoDir,
    prompts.mustGet('explore'),
    buildExploreInput(input.work),
  );
  const plan = parsePlan(out);
  if (plan.size === 0) {
    throw new Error('explore: produced no test placements');
  }
  return plan;
}

async function execute(input: AnalyzeInput, plan: Map<string, PlanEntry>): Promise<FileEdit[]> {
  const edit = async (w: FileWork): Promise<FileEdit> => {
    const p = plan.get(w.path);
    if (!p || p.testPath.trim() === '') {
      return { path: '', content: '' }; // explorer couldn't place it -> skip
    }
    let src: string;
    try {
      src = readFile(input.repoDir, w.path);
    } catch {
      // Any read error (including a path escaping the repo root) -> skip.
      return { path: '', content: '' };
    }
    const out = await generateText(
      coder(input),
      prompts.mustGet('analyze'),
      buildExecuteInput(w, src, p, input.feedback),
    );
    return { path: p.testPath, content: stripFences(out) };
  };

  return parallelAnalyze(input.work, edit);
}

export function parsePlan(out: string): Map<string, PlanEntry> {
  const js = extractJsonArray(out);
  if (js === '') {
    throw new Error('explore: no JSON array in explorer output');
  }
  let entries: unknown;
  try {
    entries = JSON.parse(js);
  } catch (err) {
    throw new Error(`explore: decode plan JSON: ${(err as Error).message}`);
  }
  const m = new Map<string, PlanEntry>();
  if (Array.isArray(entries)) {
    for (const e of entries) {
      if (!e || typeof e !== 'object') {
        continue;
      }
      const source = typeof e.source === 'string' ? e.source : '';
      if (source.trim() !== '') {
        m.set(source, {
          source,
          testPath: typeof e.test_path === 'string' ? e.test_path : '',
          framework: typeof e.framework === 'string' ? e.framework : '',
          notes: typeof e.notes === 'string' ? e.notes : '',
        });
      }
    }
  }
  return m;
}

function buildExploreInput(work: FileWork[]): string {
  const lines = ['Source files that need tests:'];
  for (const w of work) {
    lines.push(`- ${w.path}`);
  }
  return lines.join('\n') + '\n';
}

export function buildExecuteInput(
  w: FileWork,
  src: string,
  p: PlanEntry,
  ciFeedback: string,
): string {
  let body = `Write the test file at: ${p.testPath}\nFramework / convention: ${p.framework}\n`;
  if (p.notes.trim() !== '') {
    body += `Notes: ${p.notes}\n`;
  }
  body += '\nUncovered logic to cover:\n';
  for (const u of w.items) {
    body += `- ${u}\n`;
  }
  if (ciFeedback !== '') {
    body += '\nThe previous attempt failed CI with:\n' + ciFeedback + '\n';
  }
  body += `\nSource file (${w.path}):\n\`\`\`\n${src}\n\`\`\`\n`;
  return body;
}
