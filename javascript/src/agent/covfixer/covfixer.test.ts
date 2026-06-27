// Tests for covfixer: triage, explore-plan parse, two-phase analyze, and engine identity.
// The ScriptedLlm routes by system instruction (triage / explore-plan / execute); we
// assert on structure (paths, plan keys), never on LLM-authored content.
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { type AnalyzeInput, type Deps, type FileWork, NoWorkError } from '../fixflow/index';
import { FakeLlm, ScriptedLlm } from '../../testutil/fakes';
import { analyze, buildExecuteInput, parsePlan, type PlanEntry } from './analyze';
import { newCoverageEngine } from './coverage';
import { parseTriage, triage } from './triage';

describe('covfixer triage', () => {
  it('parses a triage JSON array', () => {
    const work = parseTriage(
      '[{"path":"calc.ts","uncovered":["divide error path","add edge cases"]},{"path":"","uncovered":[]}]',
    );
    expect(work).toHaveLength(1);
    expect(work[0]!.path).toBe('calc.ts');
    expect(work[0]!.items).toHaveLength(2);
  });

  it('runs triage and reports NoWorkError on an empty result', async () => {
    const work = await triage(new ScriptedLlm({ triage: '[{"path":"calc.ts","uncovered":["divide"]}]' }), 'report');
    expect(work).toHaveLength(1);
    expect(work[0]!.path).toBe('calc.ts');
    await expect(triage(new ScriptedLlm({ triage: '[]' }), 'report')).rejects.toThrow(NoWorkError);
  });
});

describe('covfixer plan', () => {
  it('parses an explorer plan', () => {
    const plan = parsePlan(
      'prose [{"source":"calc.ts","test_path":"calc.test.ts","framework":"vitest","notes":"colocated"},{"source":"","test_path":"x"}] more',
    );
    expect(plan.size).toBe(1);
    expect(plan.get('calc.ts')?.testPath).toBe('calc.test.ts');
    expect(plan.get('calc.ts')?.framework).toBe('vitest');
  });

  it('builds an execute input with all the fields', () => {
    const p: PlanEntry = { source: 'calc.ts', testPath: 'calc.test.ts', framework: 'vitest', notes: 'colocated' };
    const got = buildExecuteInput({ path: 'calc.ts', items: ['divide'] }, 'export const x = 1;', p, 'ci failed');
    for (const w of ['calc.test.ts', 'vitest', 'colocated', 'divide', 'export const x = 1;', 'ci failed']) {
      expect(got).toContain(w);
    }
  });
});

describe('covfixer analyze', () => {
  let dir: string;
  beforeEach(() => {
    dir = mkdtempSync(join(tmpdir(), 'cov-'));
  });
  afterEach(() => {
    rmSync(dir, { recursive: true, force: true });
  });

  it('plans then generates a test per file', async () => {
    writeFileSync(join(dir, 'calc.ts'), 'export function divide(a: number, b: number) { return a / b; }');
    writeFileSync(join(dir, 'existing.test.ts'), "import { it } from 'vitest';");
    const llm = new ScriptedLlm({
      plan: '[{"source":"calc.ts","test_path":"calc.test.ts","framework":"vitest","notes":"colocated"}]',
      test: "import { describe } from 'vitest';\n// covers divide\n",
    });
    const input: AnalyzeInput = {
      llm,
      codeLlm: null,
      repoDir: dir,
      work: [{ path: 'calc.ts', items: ['divide'] }] as FileWork[],
      feedback: '',
    };
    const edits = await analyze(input);
    expect(edits).toHaveLength(1);
    expect(edits[0]!.path).toBe('calc.test.ts');
    expect(edits[0]!.content).toContain('covers divide');
  });
});

describe('covfixer engine', () => {
  it('exposes the coverage identity', () => {
    const e = newCoverageEngine({ llm: new FakeLlm(), gh: {} as Deps['gh'] });
    expect(e.checkName()).toBe('agent-coverage-verify');
    // The PR label is a single config value (AGENT_PR_LABEL), not per-workflow.
    expect(e.label()).toBe('automation-agent');
  });
});
