// Deterministic unit tests for the publish + standards half: diff-hunk mapping, fingerprint
// reconciliation, CodeRabbit-style assembly, and standards discovery/distillation helpers. No model
// calls, no network — canned inputs only.
import { describe, expect, it } from 'vitest';

import type { PRFile, ReviewCommentRef, TreeEntry } from '../../githubapi/client';
import { Dimension, Severity, fingerprint, newFinding } from './findings';
import { DiffIndex, commentableLines } from './hunks';
import {
  checkConclusion,
  classify,
  findingPrefix,
  inlineCommentBody,
  reviewDetails,
  sanitizeText,
  scorecardTable,
  summaryComment,
  summaryMarker,
} from './publish';
import { fpMarker, parseFpMarker, reconcile } from './reconcile';
import { newEngine } from './reviewer';
import { Level, scoreFindings } from './scorecard';
import {
  StandardsCache,
  buildStandards,
  gateCitations,
  isEmpty,
  matchStandards,
  moduleScoped,
  parseRules,
  scopeToTouched,
  standardsCacheKey,
  standardsTools,
  touchedDirs,
} from './standards';

function prFile(partial: Partial<PRFile>): PRFile {
  return { path: '', previousPath: '', status: '', additions: 0, deletions: 0, patch: '', ...partial };
}

function treeEntry(path: string, sha: string, type = 'blob'): TreeEntry {
  return { path, sha, type };
}

describe('hunks — commentable lines', () => {
  it('maps added and context lines to head-side numbers, skips removed', () => {
    const patch = '@@ -1,3 +1,4 @@\n ctx1\n-old\n+new1\n+new2\n ctx2';
    const lines = commentableLines(patch);
    // new side: 1 ctx1, 2 new1, 3 new2, 4 ctx2 (the removed line consumes no head line)
    expect([...lines].sort((a, b) => a - b)).toEqual([1, 2, 3, 4]);
  });

  it('yields an empty set for a malformed or empty patch', () => {
    expect(commentableLines('').size).toBe(0);
    expect(commentableLines('no header here').size).toBe(0);
    expect(commentableLines('@@ bogus @@\n+x').size).toBe(0);
  });

  it('DiffIndex.inDiff reports only commentable head-side lines', () => {
    const idx = new DiffIndex([prFile({ path: 'a.ts', patch: '@@ -1 +1,2 @@\n+a\n+b' })]);
    expect(idx.inDiff('a.ts', 1)).toBe(true);
    expect(idx.inDiff('a.ts', 2)).toBe(true);
    expect(idx.inDiff('a.ts', 9)).toBe(false);
    expect(idx.inDiff('other.ts', 1)).toBe(false);
  });
});

describe('reconcile — fingerprint marker', () => {
  it('round-trips the fingerprint marker', () => {
    expect(fpMarker('a:1:msg')).toBe('<!-- ar-fp:a:1:msg -->');
    expect(parseFpMarker('body\n<!-- ar-fp:a:1:msg -->\n')).toBe('a:1:msg');
    expect(parseFpMarker('no marker')).toBe('');
  });

  it('posts new findings, keeps current ones, minimizes gone comments', () => {
    const keep = newFinding({ file: 'a.ts', line: 1, message: 'keep' });
    const add = newFinding({ file: 'a.ts', line: 2, message: 'add' });
    const existing: ReviewCommentRef[] = [
      { nodeId: 'n1', body: `x ${fpMarker(fingerprint(keep))}` }, // still applies → keep
      { nodeId: 'n2', body: `y ${fpMarker('a.ts:9:gone')}` }, // no matching finding → minimize
      { nodeId: 'n3', body: 'foreign comment, no marker' }, // ignored
    ];
    const res = reconcile([keep, add], existing);
    expect(res.toPost.map((f) => f.message)).toEqual(['add']);
    expect(res.toMinimize).toEqual(['n2']);
  });
});

describe('publish — assembly', () => {
  it('classifies findings into inline / out-of-diff / nitpicks', () => {
    const idx = new DiffIndex([prFile({ path: 'a.ts', patch: '@@ -1 +1,2 @@\n+a\n+b' })]);
    const inDiff = newFinding({ file: 'a.ts', line: 1, severity: Severity.Major, message: 'in' });
    const outside = newFinding({ file: 'a.ts', line: 99, severity: Severity.Major, message: 'out' });
    const nit = newFinding({ file: 'a.ts', line: 1, severity: Severity.Nitpick, message: 'nit' });
    const { inline, outOfDiff, nitpicks } = classify([inDiff, outside, nit], idx);
    expect(inline.map((f) => f.message)).toEqual(['in']);
    expect(outOfDiff.map((f) => f.message)).toEqual(['out']);
    expect(nitpicks.map((f) => f.message)).toEqual(['nit']);
  });

  it('renders an inline comment with suggestion, AI prompt, and fingerprint', () => {
    const f = newFinding({
      file: 'a.ts',
      line: 3,
      dimension: Dimension.Security,
      severity: Severity.Critical,
      message: 'sqli',
      suggestion: 'const q = safe();',
      fixPrompt: 'Parameterize the query.',
    });
    const body = inlineCommentBody(f);
    expect(body).toContain('🔒 Security');
    expect(body).toContain('```suggestion');
    expect(body).toContain('🤖 Prompt for AI agents');
    expect(body).toContain(fpMarker(fingerprint(f)));
  });

  it('sanitizes HTML and breaks @mentions without pinging', () => {
    const out = sanitizeText('call </details> and ping @octocat');
    expect(out).toContain('&lt;/details&gt;');
    expect(out).not.toContain('@octocat'); // a ZWSP is inserted after the @
    expect(out).toContain('@​octocat');
  });

  it('labels the finding prefix by dimension then severity', () => {
    expect(findingPrefix(newFinding({ dimension: Dimension.Security, message: 'x' }))).toBe('🔒 Security');
    expect(findingPrefix(newFinding({ severity: Severity.Major, message: 'x' }))).toBe('⚠️ Potential issue');
    expect(findingPrefix(newFinding({ severity: Severity.Medium, message: 'x' }))).toBe('🛠️ Refactor');
  });

  it('assembles the marker summary with the scorecard and collapsibles', () => {
    const card = scoreFindings([
      newFinding({ dimension: Dimension.Performance, severity: Severity.Major, message: 'slow' }),
    ]);
    const marker = summaryMarker('o', 'r', 5);
    const meta = { owner: 'o', repo: 'r', number: 5, headSha: 'abc', files: [], tiers: 't', standards: [] };
    const nit = [newFinding({ file: 'a.ts', line: 1, severity: Severity.Nitpick, message: 'nit' })];
    const outside = [newFinding({ file: 'b.ts', line: 2, severity: Severity.Major, message: 'out' })];
    const body = summaryComment(marker, card, 1, nit, outside, meta);
    expect(body.startsWith(marker)).toBe(true);
    expect(body).toContain('Agent review — Overall: Yellow · Actionable comments: 1');
    expect(body).toContain('| Dimension | Level |');
    expect(body).toContain('🧹 Nitpicks (1)');
    expect(body).toContain('🔭 Outside diff range (1)');
    expect(body).toContain('- Standards: generic review');
  });

  it('states "no findings" for an empty scorecard and lists applied standards', () => {
    expect(scorecardTable(scoreFindings([]))).toBe('_No findings._\n\n');
    const meta = { owner: 'o', repo: 'r', number: 1, headSha: 'h', files: [], tiers: '', standards: ['AGENTS.md'] };
    expect(reviewDetails(meta)).toContain('- Standards applied: AGENTS.md');
  });

  it('maps overall grade to success (green) else neutral, never failure', () => {
    expect(checkConclusion(Level.Green)).toBe('success');
    expect(checkConclusion(Level.Yellow)).toBe('neutral');
    expect(checkConclusion(Level.Red)).toBe('neutral');
  });

  it('builds the summary marker as the external contract', () => {
    expect(summaryMarker('acme', 'web', 42)).toBe('<!-- automation-agent:review:acme/web#42 -->');
  });
});

describe('standards — discovery helpers', () => {
  it('matches standards globs and scopes per-module instruction files to touched dirs', () => {
    const entries = [
      treeEntry('AGENTS.md', 's1'), // root-global → always applies
      treeEntry('src/mod/AGENTS.md', 's2'), // module-scoped
      treeEntry('other/AGENTS.md', 's3'), // module-scoped, untouched
      treeEntry('src/mod/code.ts', 's4'), // not a standards doc
    ];
    const matched = matchStandards(entries, ['AGENTS.md']);
    expect(matched.map((e) => e.path)).toEqual(['AGENTS.md', 'other/AGENTS.md', 'src/mod/AGENTS.md']);
    const scoped = scopeToTouched(matched, [prFile({ path: 'src/mod/code.ts' })]);
    expect(scoped.map((e) => e.path)).toEqual(['AGENTS.md', 'src/mod/AGENTS.md']);
  });

  it('touchedDirs walks every ancestor to the root; moduleScoped flags nested instruction files', () => {
    expect([...touchedDirs([prFile({ path: 'a/b/c.ts' })])].sort()).toEqual(['.', 'a', 'a/b']);
    expect(moduleScoped('src/AGENTS.md')).toBe(true);
    expect(moduleScoped('AGENTS.md')).toBe(false); // root is global
    expect(moduleScoped('src/CONTRIBUTING.md')).toBe(false); // not an instruction file
  });

  it('keys the cache on the matched docs blob SHAs (a changed doc misses)', () => {
    const a = standardsCacheKey('o', 'r', [treeEntry('AGENTS.md', 'sha1')]);
    const b = standardsCacheKey('o', 'r', [treeEntry('AGENTS.md', 'sha2')]);
    const c = standardsCacheKey('o', 'r', [treeEntry('AGENTS.md', 'sha1')]);
    expect(a).toBe(c);
    expect(a).not.toBe(b);
  });

  it('parseRules extracts a rule array defensively and drops id-less/dup rules', () => {
    const raw = 'noise ```json\n[{"id":"R1","dimension":"security","summary":"no secrets","source":"AGENTS.md"},{"id":"R1","summary":"dup"},{"summary":"no id"}]\n```';
    const rules = parseRules(raw);
    expect(rules).toHaveLength(1);
    expect(rules[0]!.id).toBe('R1');
    expect(rules[0]!.dimension).toBe(Dimension.Security);
    expect(parseRules('not json')).toEqual([]);
  });

  it('Standards renders a menu, validates ids, and serves full docs; isEmpty guards null', () => {
    const std = buildStandards(
      [{ id: 'R1', dimension: Dimension.PatternViolation, summary: 'wrap errors', source: 'AGENTS.md' }],
      new Map([['AGENTS.md', 'full text']]),
      ['AGENTS.md'],
    );
    expect(isEmpty(std)).toBe(false);
    expect(std!.menu()).toContain('- R1 [pattern_violation] wrap errors (source: AGENTS.md)');
    expect(std!.validId('R1')).toBe(true);
    expect(std!.validId('R9')).toBe(false);
    expect(std!.ruleDoc('R1')).toBe('full text');
    expect(std!.sourceList()).toEqual(['AGENTS.md']);
    expect(isEmpty(buildStandards([], new Map(), []))).toBe(true);
  });

  it('standardsTools binds getRule only when standards exist', () => {
    expect(standardsTools(null)).toHaveLength(0);
    const std = buildStandards(
      [{ id: 'R1', dimension: Dimension.Other, summary: 's', source: 'AGENTS.md' }],
      new Map([['AGENTS.md', 'doc']]),
      ['AGENTS.md'],
    );
    expect(standardsTools(std)).toHaveLength(1);
  });

  it('StandardsCache memoizes hit/miss including a null (rule-less) result', () => {
    const cache = new StandardsCache();
    expect(cache.get('k').ok).toBe(false);
    cache.put('k', null);
    expect(cache.get('k')).toEqual({ std: null, ok: true });
  });
});

describe('standards — citation gate', () => {
  const std = buildStandards(
    [{ id: 'R1', dimension: Dimension.PatternViolation, summary: 's', source: 'AGENTS.md' }],
    new Map([['AGENTS.md', 'doc']]),
    ['AGENTS.md'],
  );

  it('drops an uncited conformance finding when uncitedDrop is set', () => {
    const eng = newEngine({ standardsEnabled: true, uncitedDrop: true });
    const cited = newFinding({ dimension: Dimension.PatternViolation, ruleId: 'R1', message: 'ok' });
    const uncited = newFinding({ dimension: Dimension.PatternViolation, ruleId: '', message: 'no rule' });
    const security = newFinding({ dimension: Dimension.Security, message: 'stands alone' });
    const out = gateCitations(eng, [cited, uncited, security], std);
    expect(out.map((f) => f.message)).toEqual(['ok', 'stands alone']);
  });

  it('demotes an uncited conformance finding to nitpick by default', () => {
    const eng = newEngine({ standardsEnabled: true, uncitedDrop: false });
    const uncited = newFinding({ dimension: Dimension.Architecture, severity: Severity.Major, message: 'x' });
    const out = gateCitations(eng, [uncited], std);
    expect(out).toHaveLength(1);
    expect(out[0]!.severity).toBe(Severity.Nitpick);
  });

  it('passes findings through untouched when standards are off', () => {
    const eng = newEngine({ standardsEnabled: false });
    const uncited = newFinding({ dimension: Dimension.PatternViolation, ruleId: '', message: 'x' });
    expect(gateCitations(eng, [uncited], std)).toHaveLength(1);
  });
});

