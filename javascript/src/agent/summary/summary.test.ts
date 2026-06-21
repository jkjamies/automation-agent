// Tests for the summary workflow.
import { LlmAgent, ParallelAgent, SequentialAgent } from '@google/adk';
import { describe, expect, it } from 'vitest';

import type { Commit } from '../../githubapi/client';
import type { Message, Notifier } from '../../notify/notify';
import { FakeLlm } from '../../testutil/fakes';
import { drive, driveCollectState, newRunner } from '../setup/runner';
import { buildSummaryAgent } from './agentsSetup';
import {
  buildInstruction,
  type CommitLister,
  formatCommits,
  safeName,
  splitRepo,
  summaryInstruction,
} from './summary';

class FakeLister implements CommitLister {
  constructor(
    private readonly byRepo: Record<string, Commit[]> = {},
    private readonly error?: Error,
  ) {}
  async listCommitsSince(owner: string, repo: string): Promise<Commit[]> {
    if (this.error) throw this.error;
    return this.byRepo[`${owner}/${repo}`] ?? [];
  }
}

class FakeNotifier implements Notifier {
  msgs: Message[] = [];
  async notify(m: Message): Promise<void> {
    this.msgs.push(m);
  }
}

function commit(sha: string, message: string, author: string): Commit {
  return { sha, message, author, url: '', when: null };
}

describe('summary helpers', () => {
  it('formats an empty repo', () => {
    expect(formatCommits('o/r', [])).toContain('no commits');
  });

  it('formats commits with first line only', () => {
    const got = formatCommits('o/r', [commit('abcdef1234', 'fix bug\n\ndetails', 'Jane')]);
    expect(got).toBe('Repository o/r (1 commits):\n- abcdef1 fix bug (Jane)\n');
    expect(got).not.toContain('details');
  });

  it('builds a sorted, filtered instruction', () => {
    const state = { 'commits:b/b': 'repo B data', 'commits:a/a': 'repo A data', other: 'ignore me' };
    const got = buildInstruction('PROMPT', state);
    expect(got.startsWith('PROMPT')).toBe(true);
    expect(got).not.toContain('ignore me');
    expect(got.indexOf('repo A data')).toBeLessThan(got.indexOf('repo B data'));
  });

  it('notes empty commit data', () => {
    expect(buildInstruction('P', {})).toContain('no commit data');
  });

  it('reads state in the instruction provider', () => {
    const provider = summaryInstruction('PROMPT');
    const ctx = { state: { toRecord: () => ({ 'commits:o/r': 'the commit data' }) } };
    const got = provider(ctx as any);
    expect(got.startsWith('PROMPT')).toBe(true);
    expect(got).toContain('the commit data');
  });

  it('splits repos and sanitizes names', () => {
    expect(splitRepo('owner/repo')).toEqual(['owner', 'repo']);
    expect(splitRepo('bad')).toBeNull();
    expect(safeName('a/b:c')).toBe('a_b_c');
  });
});

describe('buildSummaryAgent', () => {
  it('wires the sequential workflow', () => {
    const a = buildSummaryAgent({
      llm: new FakeLlm('digest'),
      gh: new FakeLister(),
      notify: new FakeNotifier(),
      repos: ['o/r', 'a/b'],
    });
    expect(a).toBeInstanceOf(SequentialAgent);
    expect(a.name).toBe('summary_workflow');
    expect(a.subAgents).toHaveLength(3);
    const [parallel, summarizer, notifier] = a.subAgents;
    expect(parallel).toBeInstanceOf(ParallelAgent);
    expect(parallel!.name).toBe('fetch_all');
    expect(parallel!.subAgents.map((s) => s.name)).toEqual(['fetch_o_r', 'fetch_a_b']);
    expect(summarizer).toBeInstanceOf(LlmAgent);
    expect(summarizer!.name).toBe('summarizer');
    expect((summarizer as LlmAgent).outputKey).toBe('digest');
    expect(notifier!.name).toBe('notify');
  });

  it('validates its dependencies', () => {
    expect(() =>
      buildSummaryAgent({ llm: new FakeLlm(''), gh: new FakeLister(), notify: new FakeNotifier(), repos: [] }),
    ).toThrow(/at least one repo/);
    expect(() =>
      buildSummaryAgent({ llm: null as any, gh: null as any, notify: null as any, repos: ['o/r'] }),
    ).toThrow(/required/);
  });
});

describe('summary workflow behavior', () => {
  it('writes the per-repo state key', async () => {
    const gh = new FakeLister({ 'o/r': [commit('abc1234', 'do the thing', 'X')] });
    const a = buildSummaryAgent({ llm: new FakeLlm('digest'), gh, notify: new FakeNotifier(), repos: ['o/r'] });
    const runner = newRunner('summary-test', a);
    const state = await driveCollectState(runner, 'u', 's', 'go');
    expect(state['commits:o/r']).toBeDefined();
    expect(String(state['commits:o/r'])).toContain('do the thing');
  });

  it('posts the digest', async () => {
    const gh = new FakeLister({ 'o/r': [commit('abc1234', 'do the thing', 'X')] });
    const notifier = new FakeNotifier();
    const a = buildSummaryAgent({ llm: new FakeLlm('THE DIGEST'), gh, notify: notifier, repos: ['o/r'] });
    const runner = newRunner('summary-test', a);
    await drive(runner, 'u', 's', 'go');
    expect(notifier.msgs).toHaveLength(1);
    expect(notifier.msgs[0]!.title).toBe('Daily commit digest');
    expect(notifier.msgs[0]!.text).toBe('THE DIGEST');
  });

  it('posts under a custom title (weekly)', async () => {
    const gh = new FakeLister({ 'o/r': [commit('abc1234', 'do the thing', 'X')] });
    const notifier = new FakeNotifier();
    const a = buildSummaryAgent({
      llm: new FakeLlm('THE DIGEST'),
      gh,
      notify: notifier,
      repos: ['o/r'],
      title: 'Weekly commit digest',
    });
    const runner = newRunner('summary-test', a);
    await drive(runner, 'u', 's', 'go');
    expect(notifier.msgs[0]!.title).toBe('Weekly commit digest');
  });

  it('surfaces a fetch error', async () => {
    const gh = new FakeLister({}, new Error('api down'));
    const a = buildSummaryAgent({ llm: new FakeLlm(''), gh, notify: new FakeNotifier(), repos: ['o/r'] });
    const runner = newRunner('summary-test', a);
    await expect(drive(runner, 'u', 's', 'go')).rejects.toThrow(/api down/);
  });
});
