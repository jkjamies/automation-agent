/**
 * Per-file parallel analysis.
 *
 * {@link parallelAnalyze} fans out one analyzer agent per file (ADK parallel agents, each
 * writing distinct state keys so they never collide), calls the edit function for each,
 * and returns the collected non-empty edits sorted by path. State-key scheme:
 * `edit:<path>` -> new content and `path:<path>` -> target edit path.
 */
import { BaseAgent, type Event, ParallelAgent } from '@google/adk';

import { newRunner, driveCollectState } from '../setup/runner';
import { textEvent } from '../setup/events';
import { safeName } from '../setup/names';
import type { FileEdit } from './applyfix';
import type { FileWork } from './engine';

const EDIT_PREFIX = 'edit:'; // state key per file: edit:<path> -> new content
const PATH_PREFIX = 'path:'; // state key per file: path:<path> -> target edit path

/**
 * Produces the edit for one file's work: a target path (which may differ from the source
 * path — e.g. a test file) and new content. Return a zero FileEdit (empty path or
 * content) to skip this file.
 */
export type EditFunc = (work: FileWork) => Promise<FileEdit>;

/**
 * A BaseAgent that runs the edit function for one file and emits its edit as a state
 * delta (or skips / errors).
 */
class Analyzer extends BaseAgent {
  constructor(
    name: string,
    private readonly work: FileWork,
    private readonly fn: EditFunc,
  ) {
    super({ name, description: `Analyzes ${work.path}` });
  }

  protected override async *runAsyncImpl(): AsyncGenerator<Event, void> {
    const w = this.work;
    let edit: FileEdit;
    try {
      edit = await this.fn(w);
    } catch (err) {
      yield textEvent(this.name, `analyze ${w.path}: ${(err as Error).message}`);
      return;
    }
    if (edit.path === '' || edit.content.trim() === '') {
      yield textEvent(this.name, `skipped ${w.path}`);
      return;
    }
    yield textEvent(this.name, `edited ${edit.path}`, {
      [EDIT_PREFIX + w.path]: edit.content,
      [PATH_PREFIX + w.path]: edit.path,
    });
  }

  protected override async *runLiveImpl(): AsyncGenerator<Event, void> {
    // not used
  }
}

/**
 * Derive a unique sub-agent name from a path. safeName maps every non-alphanumeric char to
 * `_`, so distinct paths (e.g. `a/b.kt` and `a-b.kt`) can collapse to the same name;
 * ParallelAgent needs unique sub-agent names, so a collision gets a numeric suffix —
 * otherwise one analyzer silently shadows another and that file's edits are dropped.
 */
function uniqueAnalyzerName(seen: Map<string, number>, path: string): string {
  const base = 'analyze_' + safeName(path);
  const n = (seen.get(base) ?? 0) + 1;
  seen.set(base, n);
  return n > 1 ? `${base}_${n}` : base;
}

/**
 * Fan out one analyzer per FileWork, call `fn` for each, and return the collected
 * non-empty edits sorted by path.
 *
 * @throws Error if there is no work, or if no edits are produced.
 */
export async function parallelAnalyze(work: FileWork[], fn: EditFunc): Promise<FileEdit[]> {
  if (work.length === 0) {
    throw new Error('analyze: no files to work on');
  }
  const sorted = [...work].sort((a, b) => (a.path < b.path ? -1 : a.path > b.path ? 1 : 0));

  const seen = new Map<string, number>();
  const agents: BaseAgent[] = sorted.map((w) => new Analyzer(uniqueAnalyzerName(seen, w.path), w, fn));
  const par = new ParallelAgent({
    name: 'analyze_all',
    description: 'Per-file analysis in parallel',
    subAgents: agents,
  });
  const runner = newRunner('fix-analyze', par);
  const state = await driveCollectState(runner, 'system', 'analyze', 'Produce the edits.');

  const edits: FileEdit[] = [];
  for (const w of sorted) {
    const content = state[EDIT_PREFIX + w.path];
    const path = state[PATH_PREFIX + w.path];
    if (typeof content === 'string' && content.trim() !== '' && typeof path === 'string' && path !== '') {
      edits.push({ path, content });
    }
  }
  if (edits.length === 0) {
    throw new Error('analyze produced no edits');
  }
  return edits;
}
