/**
 * Standards-aware review — steer off the reviewed repo's own conventions.
 *
 * The reviewer steers off the conventions of the repo *under review* — `.agents/standards`,
 * `.cursor/rules`, `CLAUDE.md`, whatever that repo has, not automation-agent's own. A base-tier
 * sub-agent distills the discovered docs (heterogeneous formats) into one uniform tagged rule list;
 * the compact list is injected into every lens and a lazy `get_rule` tool serves the full text on
 * demand. All API-only (no clone).
 */

import { createHash } from 'node:crypto';
import { type BaseTool, FunctionTool } from '@google/adk';

import type { PRFile, TreeEntry } from '../../githubapi/client';
import { Type } from '../setup/genai';
import { driveText, newRunner } from '../setup/runner';
import { type GlobPattern, globToRegExp } from './filter';
import { Dimension, type Finding, Severity, normalizeDimension } from './findings';
// maxBacktickRun sits in the review <-> standards cycle; ESM resolves the function binding lazily
// (used only at call time), so a static import is safe.
import { maxBacktickRun } from './review';
import type { Engine } from './reviewer';

// The distiller drive kick; the real instruction (the distill prompt + the repo's standards docs)
// lives in the agent's system instruction.
export const DISTILL_TRIGGER = "Extract the repository's rules as the JSON array specified.";

/**
 * One distilled, dimension-tagged convention rule extracted from the reviewed repo's own standards
 * docs.
 */
export interface Rule {
  id: string;
  dimension: Dimension;
  summary: string;
  source: string; // the doc path the rule came from
}

/**
 * The distilled rule set for one repo at one docs revision: the compact rule menu injected into
 * every lens, plus the full source docs for lazy getRule drill-down.
 */
export class Standards {
  readonly rules: Rule[];
  readonly byId: Map<string, Rule>;
  readonly docs: Map<string, string>; // source path -> full doc text
  readonly sources: string[]; // distinct source paths, sorted

  constructor(rules: Rule[], byId: Map<string, Rule>, docs: Map<string, string>, sources: string[]) {
    this.rules = rules;
    this.byId = byId;
    this.docs = docs;
    this.sources = sources;
  }

  /** Report whether there are no rules to inject, so callers can fall back to generic. */
  empty(): boolean {
    return this.rules.length === 0;
  }

  /**
   * Render the compact rule list for an agent prompt: one line per rule (id, dimension, summary,
   * source). Small by construction — summaries, not full text.
   */
  menu(): string {
    if (this.empty()) {
      return '';
    }
    return this.rules
      .map((r) => `- ${r.id} [${r.dimension}] ${r.summary} (source: ${r.source})\n`)
      .join('');
  }

  /** Report whether `ruleId` is a rule in this set (the citation gate's check). */
  validId(ruleId: string): boolean {
    return this.byId.has(ruleId);
  }

  /**
   * Return the full source-doc text for a rule id, for lazy drill-down. Empty if the id is unknown
   * or its source doc is absent.
   */
  ruleDoc(ruleId: string): string {
    const r = this.byId.get(ruleId);
    if (r === undefined) {
      return '';
    }
    return this.docs.get(r.source) ?? '';
  }

  /** Return the applied source paths (empty when no standards), for the summary report. */
  sourceList(): string[] {
    return this.empty() ? [] : this.sources;
  }
}

/** Report whether a (possibly null) standards set has no rules to inject. */
export function isEmpty(std: Standards | null): boolean {
  return std === null || std.empty();
}

/**
 * Fetch and distill the reviewed repo's convention docs into a tagged rule list, cached per repo +
 * docs revision. Returns null (review generic) when standards are disabled, none are found, or
 * distillation yields nothing. Best-effort: a discovery/fetch error logs and returns null rather
 * than failing the review.
 */
export async function discoverStandards(
  engine: Engine,
  owner: string,
  repo: string,
  ref: string,
  changed: PRFile[],
): Promise<Standards | null> {
  if (!engine.standardsEnabled) {
    return null;
  }
  let entries: TreeEntry[];
  let truncated: boolean;
  try {
    ({ entries, truncated } = await engine.gh!.tree(owner, repo, ref));
  } catch (err) {
    engine.log.warn('standards: list tree failed; reviewing generic', {
      repo: `${owner}/${repo}`,
      err: errMsg(err),
    });
    return null;
  }
  if (truncated) {
    // A truncated tree (very large repo) may have missed convention files. Steering off a
    // knowingly-incomplete rule set is worse than a generic review, so degrade to generic (no
    // cache, so a later event with a complete tree retries).
    engine.log.warn('standards: repo tree truncated; reviewing generic', { repo: `${owner}/${repo}` });
    return null;
  }
  // Per-module scoping: a per-directory instruction file applies only when the PR touches its
  // module. Repo-global conventions always apply.
  const matched = scopeToTouched(matchStandards(entries, engine.standardsGlobs), changed);
  if (matched.length === 0) {
    return null;
  }
  // Cache on the matched docs' blob SHAs, so distillation runs once per standards change.
  const key = standardsCacheKey(owner, repo, matched);
  const cached = engine.standardsCache.get(key);
  if (cached.ok) {
    return cached.std;
  }

  const docs = new Map<string, string>();
  const sources: string[] = [];
  let total = 0;
  let fetchOk = true;
  for (const m of matched) {
    let content: string;
    try {
      content = await engine.gh!.getFileContent(owner, repo, m.path, ref);
    } catch (err) {
      // A transient fetch failure leaves the rule set incomplete; degrade to generic for this
      // round (and don't memoize, so a later event retries the full set).
      engine.log.warn('standards: fetch failed; reviewing generic', { path: m.path, err: errMsg(err) });
      fetchOk = false;
      break;
    }
    if (total + content.length > engine.standardsMaxBytes) {
      engine.log.warn('standards: byte cap reached; remaining docs skipped', {
        cap: engine.standardsMaxBytes,
        applied: sources.length,
      });
      break;
    }
    total += content.length;
    docs.set(m.path, content);
    sources.push(m.path);
  }
  if (!fetchOk || docs.size === 0) {
    // Incomplete discovery (a fetch failed) or nothing fetched: review generic, uncached.
    return null;
  }

  let rules: Rule[];
  try {
    rules = await distill(engine, docs, sources);
  } catch (err) {
    engine.log.warn('standards: distillation failed; reviewing generic', {
      repo: `${owner}/${repo}`,
      err: errMsg(err),
    });
    return null;
  }
  const std = buildStandards(rules, docs, sources);
  // Discovery was complete (whole tree, every matched doc fetched), so memoize — incl. a legitimate
  // empty distill, so a rule-less repo isn't re-distilled until its docs change.
  engine.standardsCache.put(key, std);
  if (isEmpty(std)) {
    engine.log.info('standards: discovered docs but distilled no rules; reviewing generic', {
      repo: `${owner}/${repo}`,
      docs: sources.length,
    });
    return null;
  }
  engine.log.info('standards: applied', {
    repo: `${owner}/${repo}`,
    rules: std!.rules.length,
    sources: std!.sources.join(', '),
  });
  return std;
}

/**
 * Return the tree's blob entries whose path matches any standards glob, sorted by path for
 * deterministic ordering and cache keys.
 */
export function matchStandards(entries: TreeEntry[], globs: string[]): TreeEntry[] {
  const pats = compileStandardsGlobs(globs);
  const out = entries.filter((en) => en.type === 'blob' && matchesGlob(pats, en.path));
  out.sort((a, b) => (a.path < b.path ? -1 : a.path > b.path ? 1 : 0));
  return out;
}

/**
 * Build path matchers from the configured globs. A glob with no '/' matches the basename; one with
 * a '/' matches the full path. Reuses the exclude-filter glob compiler.
 */
export function compileStandardsGlobs(globs: string[]): GlobPattern[] {
  const pats: GlobPattern[] = [];
  for (const raw of globs) {
    const g = raw.trim();
    if (g === '') {
      continue;
    }
    pats.push({ re: globToRegExp(g), basename: !g.includes('/') });
  }
  return pats;
}

/** Report whether `p` matches any compiled standards glob. */
export function matchesGlob(pats: GlobPattern[], p: string): boolean {
  const base = basename(p);
  for (const pat of pats) {
    const target = pat.basename ? base : p;
    if (pat.re.test(target)) {
      return true;
    }
  }
  return false;
}

/**
 * Drop per-directory instruction files (AGENTS.md/CLAUDE.md/GEMINI.md nested below the repo root)
 * for modules the PR does not touch — so a finding in one module isn't judged against another
 * module's conventions. Repo-global conventions (root files, dotfolder rule dirs, linter configs)
 * always apply.
 */
export function scopeToTouched(matched: TreeEntry[], changed: PRFile[]): TreeEntry[] {
  const touched = touchedDirs(changed);
  const out: TreeEntry[] = [];
  for (const m of matched) {
    if (moduleScoped(m.path) && !touched.has(dirname(m.path))) {
      continue;
    }
    out.push(m);
  }
  return out;
}

/**
 * Report whether a convention file is a per-directory instruction file below the repo root (applies
 * only to its own module). Root files and non-instruction conventions are repo-global.
 */
export function moduleScoped(p: string): boolean {
  const d = dirname(p);
  if (d === '' || d === '.') {
    return false;
  }
  return ['AGENTS.md', 'CLAUDE.md', 'GEMINI.md'].includes(basename(p));
}

/**
 * The set of every ancestor directory (up to the root ".") of the changed files, so a per-module
 * instruction file applies when any file in its subtree changed.
 */
export function touchedDirs(changed: PRFile[]): Set<string> {
  const dirs = new Set<string>();
  for (const f of changed) {
    let d = dirname(f.path);
    if (d === '') {
      d = '.';
    }
    for (;;) {
      dirs.add(d);
      if (d === '.') {
        break;
      }
      const parent = dirname(d);
      d = parent !== '' ? parent : '.';
    }
  }
  return dirs;
}

/**
 * Hash the repo and the matched docs' (path, blob SHA) pairs, so the cache keys on the standards
 * revision: any change to a standards file changes its blob SHA and misses.
 */
export function standardsCacheKey(owner: string, repo: string, matched: TreeEntry[]): string {
  const parts = matched.map((m) => `${m.path}:${m.sha}`).sort();
  return createHash('sha256')
    .update(`${owner}/${repo}\n` + parts.join('\n'), 'utf-8')
    .digest('hex');
}

/**
 * Run the base-tier distiller sub-agent over the discovered docs, returning the parsed rule list.
 * Best-effort: a runner/drive error propagates to the caller (which degrades to generic).
 */
export async function distill(
  engine: Engine,
  docs: Map<string, string>,
  sources: string[],
): Promise<Rule[]> {
  // Deferred import breaks the standards <-> agentsSetup module cycle.
  const agentsSetup = await import('./agentsSetup');
  const agent = agentsSetup.buildDistillerAgent(engine, docs, sources);
  const runner = newRunner('reviewer-distill', agent);
  const text = await driveText(runner, 'system', 'distill', DISTILL_TRIGGER);
  return parseRules(text);
}

/**
 * Compose the distiller's instruction: the distill prompt followed by each discovered standards
 * doc, fenced so the doc content (untrusted) can't break the prompt.
 */
export function buildDistillerInstruction(
  promptBody: string,
  docs: Map<string, string>,
  sources: string[],
): string {
  const parts: string[] = [promptBody, '\n\n## Repository standards documents\n\n'];
  for (const src of sources) {
    const doc = docs.get(src) ?? '';
    parts.push(`### Document: ${src}\n\n`);
    let fence = '`'.repeat(maxBacktickRun(doc) + 1);
    if (fence.length < 3) {
      fence = '```';
    }
    parts.push(fence + '\n');
    parts.push(doc);
    if (!doc.endsWith('\n')) {
      parts.push('\n');
    }
    parts.push(fence + '\n\n');
  }
  return parts.join('');
}

/**
 * Assemble the standards from distilled rules + the fetched docs. null when there are no rules (so
 * {@link isEmpty} and a generic fallback hold).
 */
export function buildStandards(
  rules: Rule[],
  docs: Map<string, string>,
  sources: string[],
): Standards | null {
  if (rules.length === 0) {
    return null;
  }
  const byId = new Map<string, Rule>();
  for (const r of rules) {
    byId.set(r.id, r);
  }
  return new Standards(rules, byId, docs, [...sources].sort());
}

/**
 * Extract the distilled rule list from the base model's output. Defensive by design (mirrors
 * {@link parseFindings}): it scans for the first JSON array that decodes into the rule shape,
 * tolerating fences/prose, and never raises — a garbled distillation degrades to "no rules" (a
 * generic review) rather than failing.
 */
export function parseRules(raw: string): Rule[] {
  for (let i = 0; i < raw.length; i++) {
    if (raw[i] !== '[') {
      continue;
    }
    const end = matchArrayEnd(raw, i);
    if (end < 0) {
      continue;
    }
    let value: unknown;
    try {
      value = JSON.parse(raw.slice(i, end + 1));
    } catch {
      continue;
    }
    if (!Array.isArray(value) || value.length === 0 || !validRuleArray(value)) {
      continue;
    }
    const out: Rule[] = [];
    const seen = new Set<string>();
    for (const w of value as Array<Record<string, unknown>>) {
      const ruleId = str(w.id).trim();
      const summary = str(w.summary).trim();
      if (ruleId === '' || summary === '' || seen.has(ruleId)) {
        continue; // a rule needs a unique id and a summary to be usable
      }
      seen.add(ruleId);
      out.push({
        id: ruleId,
        dimension: normalizeDimension(str(w.dimension)),
        summary,
        source: str(w.source).trim(),
      });
    }
    if (out.length > 0) {
      return out;
    }
  }
  return [];
}

/**
 * Report whether every element decodes cleanly into the rule shape: an object whose known string
 * fields are strings. A type mismatch fails the whole array so the scan moves on.
 */
function validRuleArray(value: unknown[]): boolean {
  for (const el of value) {
    if (typeof el !== 'object' || el === null || Array.isArray(el)) {
      return false;
    }
    const obj = el as Record<string, unknown>;
    for (const key of ['id', 'dimension', 'summary', 'source']) {
      if (key in obj && typeof obj[key] !== 'string') {
        return false;
      }
    }
  }
  return true;
}

/**
 * Return the lazy getRule drill-down tool bound to this run's rule set, or an empty list when there
 * are no standards (the lenses then run without it). The compact rule menu lives in the prompt;
 * full text is fetched on demand.
 */
export function standardsTools(std: Standards | null): BaseTool[] {
  if (isEmpty(std)) {
    return [];
  }
  const real = std!;
  const getRule = new FunctionTool({
    name: 'get_rule',
    description:
      'Return the full source text of a repo standard rule by its id (e.g. "R3") so you can read the exact wording before flagging a conformance issue.',
    parameters: {
      type: Type.OBJECT,
      properties: { id: { type: Type.STRING, description: 'the rule id, e.g. "R3"' } },
      required: ['id'],
    },
    // Self-wrap so a bad id is a recoverable tool error, not a thrown rejection that aborts the run.
    execute: (input) => {
      try {
        return { rule: real.ruleDoc(str((input as { id: unknown }).id).trim()) };
      } catch (err) {
        return { error: errMsg(err) };
      }
    },
  });
  return [getRule];
}

// The lenses whose findings assert "this violates the repo's documented standard" — they must cite
// a real injected rule_id. Other dimensions (e.g. security) stand on their own.
export const CONFORMANCE_DIMENSIONS: ReadonlySet<Dimension> = new Set<Dimension>([
  Dimension.PatternViolation,
  Dimension.Architecture,
]);

/**
 * Enforce that a conformance finding (pattern/architecture) is anchored to one of the repo's own
 * injected rules: an empty or unknown rule_id is dropped or demoted to nitpick per
 * REVIEW_UNCITED_MODE. When standards-awareness is off, findings pass through untouched.
 */
export function gateCitations(engine: Engine, findings: Finding[], std: Standards | null): Finding[] {
  if (!engine.standardsEnabled || isEmpty(std)) {
    return findings;
  }
  const real = std!;
  const out: Finding[] = [];
  for (let f of findings) {
    if (CONFORMANCE_DIMENSIONS.has(f.dimension) && !real.validId(f.ruleId)) {
      if (engine.uncitedDrop) {
        continue;
      }
      f = { ...f, severity: Severity.Nitpick }; // demote an unanchored "violation"
    }
    out.push(f);
  }
  return out;
}

/**
 * Memoizes distilled rule sets per repo + docs revision (in-memory; a cold start re-distills). A
 * cached null means "discovered docs, distilled nothing" and is retained so a generic repo isn't
 * re-distilled until its docs change.
 */
export class StandardsCache {
  private readonly m = new Map<string, Standards | null>();

  get(key: string): { std: Standards | null; ok: boolean } {
    if (this.m.has(key)) {
      return { std: this.m.get(key) ?? null, ok: true };
    }
    return { std: null, ok: false };
  }

  put(key: string, std: Standards | null): void {
    this.m.set(key, std);
  }
}

/** The final path segment (basename), splitting on '/' as posix paths do. */
function basename(p: string): string {
  const idx = p.lastIndexOf('/');
  return idx < 0 ? p : p.slice(idx + 1);
}

/** The parent directory of a posix path: "" for a bare name, else everything before the last '/'. */
function dirname(p: string): string {
  const idx = p.lastIndexOf('/');
  return idx < 0 ? '' : p.slice(0, idx);
}

/** Scan `raw` for the `]` that closes the `[` at `start`, respecting string literals and escapes. */
function matchArrayEnd(raw: string, start: number): number {
  let depth = 0;
  let inString = false;
  let escaped = false;
  for (let i = start; i < raw.length; i++) {
    const ch = raw[i];
    if (inString) {
      if (escaped) {
        escaped = false;
      } else if (ch === '\\') {
        escaped = true;
      } else if (ch === '"') {
        inString = false;
      }
      continue;
    }
    if (ch === '"') {
      inString = true;
    } else if (ch === '[') {
      depth++;
    } else if (ch === ']') {
      depth--;
      if (depth === 0) {
        return i;
      }
    }
  }
  return -1;
}

/** Coerce a possibly-missing string field to `""`. */
function str(v: unknown): string {
  return typeof v === 'string' ? v : '';
}

/** Extract a message from a thrown value. */
function errMsg(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}
