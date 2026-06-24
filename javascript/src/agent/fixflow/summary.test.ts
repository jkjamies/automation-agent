// Tests for the status-aware terminal summary builder (pure, no I/O).
import { describe, expect, it } from 'vitest';

import type { Comparison } from '../../githubapi/client';
import { type SummaryInput, TerminalOutcome, buildSummaryText } from './summary';

function input(overrides: Partial<SummaryInput> = {}): SummaryInput {
  return {
    outcome: TerminalOutcome.Success,
    workflow: 'lint',
    fullRepo: 'acme/api',
    prNumber: 42,
    attempts: 1,
    report: '',
    lastOutput: '',
    timeout: '90m',
    checkName: 'agent-lint-verify',
    changed: { totalCommits: 0, files: [] },
    ...overrides,
  };
}

const changed: Comparison = {
  totalCommits: 2,
  files: [
    { path: 'a.ts', status: 'modified', additions: 1, deletions: 0 },
    { path: 'b.ts', status: 'added', additions: 5, deletions: 0 },
  ],
};

describe('buildSummaryText', () => {
  it('frames a success with the change summary and targeted findings', () => {
    const text = buildSummaryText(
      input({ outcome: TerminalOutcome.Success, attempts: 2, report: 'fix unused import', changed }),
    );
    expect(text).toContain('the lint fix passed CI after 2 attempts.');
    expect(text).toContain('2 commits changed a.ts, b.ts.');
    expect(text).toContain('Targeted: fix unused import');
  });

  it('uses the singular attempt phrasing', () => {
    const text = buildSummaryText(input({ outcome: TerminalOutcome.Success, attempts: 1 }));
    expect(text).toContain('after 1 attempt.');
  });

  it('frames an exhausted run with the remaining findings', () => {
    const text = buildSummaryText(
      input({ outcome: TerminalOutcome.Exhausted, attempts: 3, lastOutput: 'still failing: x' }),
    );
    expect(text).toContain('the lint fix still fails CI after 3 attempts. Please review.');
    expect(text).toContain('Remaining: still failing: x');
  });

  it('frames a timeout with the awaited check name and timeout', () => {
    const text = buildSummaryText(
      input({ outcome: TerminalOutcome.Timeout, attempts: 1, report: 'r', timeout: '90m' }),
    );
    expect(text).toContain('saw no CI result after 90m waiting for agent-lint-verify (1 attempt).');
    expect(text).toContain('Please review.');
  });

  it('reports no changes when the comparison is empty', () => {
    const text = buildSummaryText(input({ changed: { totalCommits: 0, files: [] } }));
    expect(text).toContain('No changes were recorded on the PR.');
  });

  it('truncates a long file list with a (+N more) suffix', () => {
    const files = Array.from({ length: 10 }, (_, i) => ({
      path: `f${i}.ts`,
      status: 'modified',
      additions: 1,
      deletions: 0,
    }));
    const text = buildSummaryText(input({ changed: { totalCommits: 1, files } }));
    expect(text).toContain('1 commit changed f0.ts, f1.ts, f2.ts, f3.ts, f4.ts, f5.ts, f6.ts, f7.ts (+2 more).');
  });

  it('collapses whitespace and truncates an overlong findings blob', () => {
    const blob = 'a'.repeat(300) + '\n\n   ' + 'b'.repeat(50);
    const text = buildSummaryText(input({ outcome: TerminalOutcome.Success, report: blob }));
    const line = text.split('\n').find((l) => l.startsWith('Targeted: '))!;
    // 280 runes + the ellipsis, after the "Targeted: " label.
    expect([...line.slice('Targeted: '.length)]).toHaveLength(281);
    expect(line.endsWith('…')).toBe(true);
  });

  it('omits the findings line when the blob is empty', () => {
    const text = buildSummaryText(input({ outcome: TerminalOutcome.Success, report: '' }));
    expect(text).not.toContain('Targeted:');
  });
});
