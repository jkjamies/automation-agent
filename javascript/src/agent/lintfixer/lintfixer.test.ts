// Tests for lintfixer: triage parsing, analyze edits, and engine identity.
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { describe, expect, it } from 'vitest';

import { type AnalyzeInput, type Deps, NoWorkError } from '../fixflow/index';
import { FakeLlm } from '../../testutil/fakes';
import { analyze, buildFilePrompt } from './analyze';
import { newLintEngine } from './lint';
import { parseTriage, triage } from './triage';

describe('lintfixer triage', () => {
  it('parses a triage JSON array', () => {
    const work = parseTriage(
      'x [{"path":"a.ts","problems":["unchecked error"]},{"path":"","problems":[]}] y',
    );
    expect(work).toHaveLength(1);
    expect(work[0]!.path).toBe('a.ts');
    expect(work[0]!.items).toHaveLength(1);
  });

  it('runs triage and reports NoWorkError on an empty result', async () => {
    const work = await triage(new FakeLlm('[{"path":"a.ts","problems":["x"]}]'), 'report');
    expect(work).toHaveLength(1);
    expect(work[0]!.path).toBe('a.ts');
    await expect(triage(new FakeLlm('[]'), 'report')).rejects.toThrow(NoWorkError);
  });
});

describe('lintfixer analyze', () => {
  it('builds a file prompt with content, problems and feedback', () => {
    const p = buildFilePrompt({ path: 'a.ts', items: ['unchecked error'] }, 'export const a = 1;', 'ci failed');
    for (const want of ['a.ts', 'unchecked error', 'export const a = 1;', 'ci failed']) {
      expect(p).toContain(want);
    }
  });

  it('rewrites each affected file', async () => {
    const dir = mkdtempSync(join(tmpdir(), 'lint-'));
    try {
      writeFileSync(join(dir, 'a.ts'), 'export const a = 1;');
      const input: AnalyzeInput = {
        llm: new FakeLlm('export const a = 2;\n'),
        codeLlm: null,
        repoDir: dir,
        work: [{ path: 'a.ts', items: ['x'] }],
        feedback: '',
      };
      const edits = await analyze(input);
      expect(edits).toHaveLength(1);
      expect(edits[0]!.path).toBe('a.ts');
      expect(edits[0]!.content).toBe('export const a = 2;\n');
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });
});

describe('lintfixer engine', () => {
  it('exposes the lint identity', () => {
    const e = newLintEngine({ llm: new FakeLlm(), gh: {} as Deps['gh'] });
    expect(e.checkName()).toBe('agent-lint-verify');
    expect(e.label()).toBe('automation-agent');
  });
});
