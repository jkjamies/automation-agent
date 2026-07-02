// Deterministic unit tests for the reviewer's pure helpers: findings parsing/normalization, the
// exclude filter, the size gate, category selection, the count-based scorecard, the glue gates, and
// the coalesce/enqueue math. No model calls, no network — canned inputs only.
import { describe, expect, it } from 'vitest';

import type { PRFile } from '../../githubapi/client';
import { newEnvelope, Kind } from '../../ingest/envelope';
import { CATEGORIES, hasUiFiles, selectCategories } from './categories';
import { coalesceKey, enqueueOptions } from './enqueue';
import {
  Dimension,
  Severity,
  clampConfidence,
  clampThreshold,
  fingerprint,
  findingsJson,
  newFinding,
  normalizeDimension,
  normalizeMessage,
  normalizeSeverity,
  parseFindings,
  severityRank,
} from './findings';
import { AVG_DIFF_LINE_BYTES, FileFilter, globToRegExp, patchBytes } from './filter';
import { dedupe, demoteToNitpick, dropLowConfidence } from './glue';
import { Level, dimLevel, levelGlyph, levelWord, scoreFindings } from './scorecard';
import { oversize } from './sizegate';

function file(partial: Partial<PRFile>): PRFile {
  return { path: '', previousPath: '', status: '', additions: 0, deletions: 0, patch: '', ...partial };
}

describe('findings parsing', () => {
  it('extracts a JSON array wrapped in fences and prose', () => {
    const raw = 'Here you go:\n```json\n[{"file":"a.go","line":3,"dimension":"security","severity":"critical","message":"sqli","confidence":0.9}]\n```\nDone.';
    const out = parseFindings(raw);
    expect(out).toHaveLength(1);
    expect(out[0]!.file).toBe('a.go');
    expect(out[0]!.dimension).toBe(Dimension.Security);
    expect(out[0]!.severity).toBe(Severity.Critical);
    expect(out[0]!.confidence).toBe(0.9);
  });

  it('treats a malformed body as no findings', () => {
    expect(parseFindings('not json at all')).toEqual([]);
    expect(parseFindings('[unterminated')).toEqual([]);
    expect(parseFindings('')).toEqual([]);
  });

  it('skips an empty array in favor of a following populated one', () => {
    const raw = 'first [] then [{"file":"b.go","message":"note"}]';
    const out = parseFindings(raw);
    expect(out).toHaveLength(1);
    expect(out[0]!.file).toBe('b.go');
  });

  it('drops a finding with no message', () => {
    expect(parseFindings('[{"file":"a.go","line":1}]')).toEqual([]);
  });

  it('rejects a non-finite confidence in the validation layer', () => {
    // NaN/Infinity are not valid JSON tokens; a string "NaN" fails the number check, so the whole
    // array is rejected and the scan finds no other array — no findings.
    expect(parseFindings('[{"message":"x","confidence":"NaN"}]')).toEqual([]);
    expect(parseFindings('[{"message":"x","confidence":true}]')).toEqual([]);
  });

  it('rejects a non-integer line in the validation layer', () => {
    expect(parseFindings('[{"message":"x","line":1.5}]')).toEqual([]);
  });

  it('normalizes unknown severity and dimension to safe defaults', () => {
    const out = parseFindings('[{"message":"x","severity":"bogus","dimension":"weird"}]');
    expect(out[0]!.severity).toBe(Severity.Nitpick);
    expect(out[0]!.dimension).toBe(Dimension.Other);
  });
});

describe('findings normalization + fingerprint', () => {
  it('folds spaces and hyphens in a dimension', () => {
    expect(normalizeDimension('Runtime Safety')).toBe(Dimension.RuntimeSafety);
    expect(normalizeDimension('error-handling')).toBe(Dimension.ErrorHandling);
  });

  it('maps known severities and defaults the rest to nitpick', () => {
    expect(normalizeSeverity('CRITICAL')).toBe(Severity.Critical);
    expect(normalizeSeverity('')).toBe(Severity.Nitpick);
  });

  it('orders severities so worse ranks higher', () => {
    expect(severityRank(Severity.Critical)).toBeGreaterThan(severityRank(Severity.Major));
    expect(severityRank(Severity.Medium)).toBeGreaterThan(severityRank(Severity.Nitpick));
  });

  it('fingerprints on file+line+normalized message, ignoring dimension', () => {
    const a = newFinding({ file: 'a.go', line: 5, message: 'Nil  Deref', dimension: Dimension.Security });
    const b = newFinding({ file: 'a.go', line: 5, message: 'nil deref', dimension: Dimension.RuntimeSafety });
    expect(fingerprint(a)).toBe(fingerprint(b));
    expect(normalizeMessage('  Foo   BAR ')).toBe('foo bar');
  });

  it('renders findings as compact JSON with the wire keys', () => {
    const json = findingsJson([newFinding({ file: 'a.go', fixPrompt: 'fix', ruleId: 'R1' })]);
    expect(json).toContain('"fix_prompt":"fix"');
    expect(json).toContain('"rule_id":"R1"');
    expect(json).not.toContain(' '); // compact
  });
});

describe('confidence clamps', () => {
  it('clampConfidence treats zero/absent as 0.5 and has NO NaN guard', () => {
    expect(clampConfidence(0)).toBe(0.5);
    expect(clampConfidence(-1)).toBe(0.5);
    expect(clampConfidence(2)).toBe(1);
    expect(clampConfidence(0.7)).toBe(0.7);
    expect(Number.isNaN(clampConfidence(NaN))).toBe(true); // no NaN guard
  });

  it('clampThreshold folds NaN and negatives to 0 (keep all)', () => {
    expect(clampThreshold(NaN)).toBe(0);
    expect(clampThreshold(-1)).toBe(0);
    expect(clampThreshold(2)).toBe(1);
    expect(clampThreshold(0.6)).toBe(0.6);
  });
});

describe('exclude filter', () => {
  it('compiles globs and matches basenames vs full paths', () => {
    const f = new FileFilter(['*.min.js', 'vendor/**', 'go.sum', '']);
    expect(f.excluded('a/b/app.min.js')).toBe(true);
    expect(f.excluded('vendor/x/y.go')).toBe(true);
    expect(f.excluded('go.sum')).toBe(true);
    expect(f.excluded('src/main.go')).toBe(false);
  });

  it('totals patch bytes on the kept set only', () => {
    const { kept, diffBytes } = new FileFilter(['go.sum']).apply([
      file({ path: 'main.go', patch: '12345' }),
      file({ path: 'go.sum', patch: 'ignored' }),
    ]);
    expect(kept).toHaveLength(1);
    expect(diffBytes).toBe(5);
  });

  it('charges an omitted patch from its line counts, binaries nothing', () => {
    expect(patchBytes(file({ path: 'a', patch: '', additions: 2, deletions: 1 }))).toBe(3 * AVG_DIFF_LINE_BYTES);
    expect(patchBytes(file({ path: 'logo.png', patch: '' }))).toBe(0);
  });

  it('escapes regexp metacharacters and treats ** vs * distinctly', () => {
    expect(globToRegExp('a.b').test('axb')).toBe(false); // '.' is literal
    expect(globToRegExp('a.b').test('a.b')).toBe(true);
    expect(globToRegExp('*.go').test('x/y.go')).toBe(false); // '*' does not cross '/'
    expect(globToRegExp('**.go').test('x/y.go')).toBe(true); // '**' crosses '/'
  });
});

describe('size gate', () => {
  it('denies over either dimension and disables a non-positive cap', () => {
    expect(oversize(60, 10, 50, 0).denied).toBe(true);
    expect(oversize(1, 2000, 50, 1000).denied).toBe(true);
    expect(oversize(1, 10, 50, 1000).denied).toBe(false);
    expect(oversize(9999, 9999, 0, 0).denied).toBe(false); // both dims disabled
  });
});

describe('category selection', () => {
  it('gates accessibility to UI/markup diffs', () => {
    expect(hasUiFiles([file({ path: 'a.go' })])).toBe(false);
    expect(hasUiFiles([file({ path: 'ui/App.tsx' })])).toBe(true);
    const noUi = selectCategories([file({ path: 'a.go' })]).map((c) => c.name);
    expect(noUi).not.toContain('accessibility');
    const withUi = selectCategories([file({ path: 'a.go' }), file({ path: 'x.css' })]).map((c) => c.name);
    expect(withUi).toContain('accessibility');
  });

  it('always includes the other catch-all and never marks a dotfile as UI', () => {
    expect(CATEGORIES.some((c) => c.other)).toBe(true);
    expect(hasUiFiles([file({ path: '.tsx' })])).toBe(false); // leading dot = no extension
  });
});

describe('scorecard', () => {
  it('derives per-dimension levels from severity counts', () => {
    expect(dimLevel({ dimension: Dimension.Security, critical: 1, major: 0, medium: 0, nitpick: 0, level: Level.Green })).toBe(Level.Red);
    expect(dimLevel({ dimension: Dimension.Security, critical: 0, major: 2, medium: 0, nitpick: 0, level: Level.Green })).toBe(Level.Red);
    expect(dimLevel({ dimension: Dimension.Security, critical: 0, major: 1, medium: 0, nitpick: 0, level: Level.Green })).toBe(Level.Yellow);
    expect(dimLevel({ dimension: Dimension.Security, critical: 0, major: 0, medium: 3, nitpick: 0, level: Level.Green })).toBe(Level.Yellow);
    expect(dimLevel({ dimension: Dimension.Security, critical: 0, major: 0, medium: 1, nitpick: 9, level: Level.Green })).toBe(Level.Green);
  });

  it('caps overall to red on any critical in a critical dimension', () => {
    const card = scoreFindings([
      newFinding({ dimension: Dimension.Security, severity: Severity.Critical, message: 'x' }),
    ]);
    expect(card.overall).toBe(Level.Red);
    expect(card.total).toBe(1);
  });

  it('otherwise takes the worst dimension level (no critical-dim cap)', () => {
    // A single major in a non-critical dimension is yellow, not red.
    const card = scoreFindings([
      newFinding({ dimension: Dimension.Performance, severity: Severity.Major, message: 'x' }),
    ]);
    expect(card.overall).toBe(Level.Yellow);
    expect(card.dims).toHaveLength(1);
  });

  it('renders level glyphs and words', () => {
    expect(levelGlyph(Level.Red)).toBe('🔴');
    expect(levelGlyph(Level.Yellow)).toBe('🟡');
    expect(levelGlyph(Level.Green)).toBe('🟢');
    expect(levelWord(Level.Red)).toBe('Red');
    expect(levelWord(Level.Yellow)).toBe('Yellow');
    expect(levelWord(Level.Green)).toBe('Green');
  });
});

describe('glue gates', () => {
  it('drops below the minimum confidence, keeps all when non-positive', () => {
    const fs = [newFinding({ message: 'a', confidence: 0.4 }), newFinding({ message: 'b', confidence: 0.8 })];
    expect(dropLowConfidence(fs, 0.6)).toHaveLength(1);
    expect(dropLowConfidence(fs, 0)).toHaveLength(2);
  });

  it('collapses duplicates by fingerprint keeping the worst severity', () => {
    const merged = dedupe([
      newFinding({ file: 'a.go', line: 1, message: 'x', severity: Severity.Nitpick, confidence: 0.9 }),
      newFinding({ file: 'a.go', line: 1, message: 'x', severity: Severity.Critical, confidence: 0.5 }),
    ]);
    expect(merged).toHaveLength(1);
    expect(merged[0]!.severity).toBe(Severity.Critical);
  });

  it('breaks a severity tie by higher confidence', () => {
    const merged = dedupe([
      newFinding({ file: 'a.go', line: 1, message: 'x', severity: Severity.Major, confidence: 0.5 }),
      newFinding({ file: 'a.go', line: 1, message: 'x', severity: Severity.Major, confidence: 0.9 }),
    ]);
    expect(merged[0]!.confidence).toBe(0.9);
  });

  it('demotes every finding to nitpick', () => {
    const out = demoteToNitpick([newFinding({ message: 'x', severity: Severity.Critical })]);
    expect(out[0]!.severity).toBe(Severity.Nitpick);
  });
});

describe('enqueue coalescing', () => {
  function reviewEnvelope(action: string, at: Date) {
    const body = `{"action":"${action}","pull_request":{"number":7,"head":{"ref":"x","sha":"s"}},"repository":{"full_name":"acme/web.api"}}`;
    return newEnvelope(Kind.Review, 'webhook:/github', Buffer.from(body), at);
  }

  it('debounces a synchronize push under a per-PR dedup name', () => {
    const opts = enqueueOptions(reviewEnvelope('synchronize', new Date(1_700_000_000_000)), 30_000);
    expect(opts.delayMs).toBe(30_000);
    expect(opts.name).toMatch(/^review-[A-Za-z0-9_-]+-7-\d+$/);
  });

  it('coalesces pushes in one window and separates a later window', () => {
    const base = 1_700_000_000_000;
    const a = enqueueOptions(reviewEnvelope('synchronize', new Date(base + 2_000)), 30_000);
    const b = enqueueOptions(reviewEnvelope('synchronize', new Date(base + 5_000)), 30_000);
    const c = enqueueOptions(reviewEnvelope('synchronize', new Date(base + 45_000)), 30_000);
    expect(a.name).toBe(b.name);
    expect(a.name).not.toBe(c.name);
  });

  it('enqueues opened/reopened/ready_for_review immediately', () => {
    for (const action of ['opened', 'reopened', 'ready_for_review']) {
      expect(enqueueOptions(reviewEnvelope(action, new Date(0)), 30_000)).toEqual({});
    }
  });

  it('yields no options for a non-review kind, unparseable payload, or non-positive debounce', () => {
    const notReview = newEnvelope(Kind.CI, 'webhook:/github', Buffer.from('{}'), new Date(0));
    expect(enqueueOptions(notReview, 30_000)).toEqual({});
    const bad = newEnvelope(Kind.Review, 'webhook:/github', Buffer.from('{not json'), new Date(0));
    expect(enqueueOptions(bad, 30_000)).toEqual({});
    expect(enqueueOptions(reviewEnvelope('synchronize', new Date(0)), 0)).toEqual({});
  });

  it('encodes the repo losslessly so near-identical repos do not collide', () => {
    const bucket = 1_700_000_000_000_000_000n;
    const a = coalesceKey({ action: '', number: 7, repoFullName: 'acme/web.api', headRef: '', headSha: '', baseRef: '', draft: false, labels: [], authorLogin: '' }, bucket);
    const b = coalesceKey({ action: '', number: 7, repoFullName: 'acme/web-api', headRef: '', headSha: '', baseRef: '', draft: false, labels: [], authorLogin: '' }, bucket);
    expect(a).not.toBe(b);
  });
});
