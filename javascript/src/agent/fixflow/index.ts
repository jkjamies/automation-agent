/**
 * fixflow — the reusable event-driven PR-fix engine (kickoff -> suspend -> CI resume).
 *
 * Concrete agents (lintfixer, covfixer) supply a {@link Spec}; the engine owns the loop,
 * the apply mechanics, attempt counting, and the in-memory parked-run registry.
 */
export { type EditFunc, parallelAnalyze } from './analyze';
export {
  type ApplyConfig,
  type ApplyResult,
  type FileEdit,
  type GitHub,
  applyFix,
  commit,
  openRepo,
} from './applyfix';
export {
  type AnalyzeFunc,
  type AnalyzeInput,
  type Deps,
  type Logger,
  type ResolvedDeps,
  type ResumeInput,
  type Spec,
  type TriageFunc,
  type FileWork,
  Engine,
  coder,
  newEngine,
  pullUrl,
} from './engine';
export { type RunParams, Driver } from './driver';
export { Kickoff, parseKickoff } from './envelope';
export { explore } from './explore';
export { readFile, safeJoin } from './files';
export { type ParkedRun, RunRegistry } from './registry';
export { listDirEntries, repoTools } from './tools';
export { extractJsonArray, extractJsonObject, stripFences } from './util';
