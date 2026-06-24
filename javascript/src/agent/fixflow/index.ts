/**
 * fixflow — the reusable event-driven PR-fix engine (kickoff -> suspend -> CI resume).
 *
 * Concrete agents (lintfixer, covfixer) supply a {@link Spec}; the engine owns the loop,
 * the apply mechanics, and attempt counting. Suspended-run state lives in a ParkStore
 * (see agent/setup/parkstore), memory by default or a durable backend.
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
export { type SummaryInput, TerminalOutcome, buildSummaryText } from './summary';
export { explore } from './explore';
export { readFile, safeJoin } from './files';
export { listDirEntries, repoTools } from './tools';
export { extractJsonArray, extractJsonObject, stripFences } from './util';
