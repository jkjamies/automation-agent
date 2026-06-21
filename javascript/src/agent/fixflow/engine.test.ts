// Tests for the engine: the full kickoff -> park -> resume loop driven through the real
// LongRunDriver, with fake (non-LLM) triage/analyze, a seed remote, a fake GitHub, and a
// fake Notifier.
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { simpleGit } from 'simple-git';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import type { PR, PRInput } from '../../githubapi/client';
import type { Message, Notifier } from '../../notify/notify';
import { FakeLlm } from '../../testutil/fakes';
import type { GitHub } from './applyfix';
import { type Deps, Engine, type FileEdit, type FileWork, type Spec, newEngine } from './engine';

class FakeGH implements GitHub {
  created: PRInput | null = null;
  labeled: string[] = [];
  existing: PR[];
  constructor(existing: PR[] = []) {
    this.existing = existing;
  }
  async findAgentPrs(): Promise<PR[]> {
    return this.existing;
  }
  async createPr(_o: string, _r: string, input: PRInput): Promise<PR> {
    this.created = input;
    return { number: 42, title: input.title, branch: input.head, headSha: '', url: 'https://gh/pr/42', labels: [] };
  }
  async addLabels(_o: string, _r: string, _n: number, ...labels: string[]): Promise<void> {
    this.labeled.push(...labels);
  }
}

class FakeNotifier implements Notifier {
  msgs: Message[] = [];
  async notify(m: Message): Promise<void> {
    this.msgs.push(m);
  }
}

let root: string;
beforeEach(() => {
  root = mkdtempSync(join(tmpdir(), 'ff-engine-'));
});
afterEach(() => {
  rmSync(root, { recursive: true, force: true });
});

async function seedRemote(name = 'remote'): Promise<string> {
  const dir = join(root, name);
  mkdirSync(dir);
  const g = simpleGit(dir);
  await g.init();
  await g.addConfig('user.name', 'seed');
  await g.addConfig('user.email', 's@x');
  writeFileSync(join(dir, 'README.md'), 'hi');
  await g.add('README.md');
  await g.commit('init');
  return dir;
}

const triage = async (): Promise<FileWork[]> => [{ path: 'a.ts', items: ['x'] }];
const analyze = async (): Promise<FileEdit[]> => [{ path: 'a.ts', content: 'export const a = 1;\n' }];

function spec(): Spec {
  return {
    name: 'test',
    branch: 'agent/fix',
    label: 'automation-agent',
    checkName: 'agent-test-verify',
    commitMessage: 'fix',
    prTitle: 'Fix',
    successTitle: 'Fix succeeded',
    reviewTitle: 'Needs human review',
    triage,
    analyze,
  };
}

function newEngineFor(remote: string, gh: FakeGH, n: FakeNotifier, s: Spec = spec()): Engine {
  const deps: Deps = {
    llm: new FakeLlm(),
    gh,
    notify: n,
    maxIter: 3,
    ciTimeoutMs: 3_600_000,
    cloneUrl: () => remote,
  };
  return newEngine(s, deps);
}

function checkBody(conclusion: string, pr: number, output = ''): string {
  return JSON.stringify({
    action: 'completed',
    check_run: {
      name: 'agent-test-verify',
      status: 'completed',
      conclusion,
      pull_requests: [{ number: pr, head: { ref: 'agent/fix' } }],
      output: { text: output },
    },
    repository: { full_name: 'acme/api' },
  });
}

describe('Engine', () => {
  it('parks on kickoff', async () => {
    const gh = new FakeGH();
    const e = newEngineFor(await seedRemote(), gh, new FakeNotifier());
    await e.kickoff('{"repo":"acme/api","base":"main","report":"r"}');
    expect(gh.created?.head).toBe('agent/fix');
    expect(gh.labeled).toHaveLength(1);
    expect(e.driver.reg.size()).toBe(1);
  });

  it('notifies success and clears the run on a passing resume', async () => {
    const n = new FakeNotifier();
    const e = newEngineFor(await seedRemote(), new FakeGH(), n);
    await e.kickoff('{"repo":"acme/api","base":"main","report":"r"}');
    await e.resume(checkBody('success', 42));
    expect(n.msgs).toHaveLength(1);
    expect(n.msgs[0]!.title).toContain('succeeded');
    expect(e.driver.reg.size()).toBe(0);
  });

  it('asks for review when attempts are exhausted', async () => {
    const n = new FakeNotifier();
    const e = newEngineFor(await seedRemote(), new FakeGH(), n);
    e.driver.reg.park('acme/api#42', { sessionId: 'run-x', callId: 'c', attempts: 3 }, 3_600_000, (k) =>
      e.driver.onTimeout(k),
    );
    await e.resume(checkBody('failure', 42, 'still broken'));
    expect(n.msgs).toHaveLength(1);
    expect(n.msgs[0]!.title).toContain('review');
    expect(e.driver.reg.size()).toBe(0);
  });

  it('retries and re-parks on a failing resume below the limit', async () => {
    const remote = await seedRemote();
    const gh = new FakeGH();
    const n = new FakeNotifier();
    const s = spec();
    const e = newEngineFor(remote, gh, n, s);
    await e.kickoff('{"repo":"acme/api","base":"main","report":"r"}');

    s.analyze = async () => [{ path: 'a.ts', content: 'export const a = 1;\n// retry\n' }];
    gh.existing = [{ number: 42, title: '', branch: 'agent/fix', headSha: '', url: '', labels: [] }];
    gh.created = null;

    await e.resume(checkBody('failure', 42, 'still failing'));
    expect(gh.created).toBeNull(); // reused, not created
    expect(n.msgs).toHaveLength(0);
    expect(e.driver.reg.size()).toBe(1);
  });

  it('exhausts after maxIter failures through the full loop', async () => {
    const remote = await seedRemote();
    const gh = new FakeGH([{ number: 42, title: '', branch: 'agent/fix', headSha: '', url: '', labels: [] }]);
    const n = new FakeNotifier();
    const s = spec();
    let calls = 0;
    s.analyze = async () => {
      calls += 1;
      return [{ path: 'a.ts', content: `export const a = ${calls};\n` }];
    };
    const e = newEngineFor(remote, gh, n, s);

    await e.kickoff('{"repo":"acme/api","base":"main","report":"r"}');
    for (let i = 0; i < 2; i++) {
      await e.resume(checkBody('failure', 42, 'boom'));
      expect(n.msgs).toHaveLength(0);
      expect(e.driver.reg.size()).toBe(1);
    }
    await e.resume(checkBody('failure', 42, 'boom'));
    expect(n.msgs).toHaveLength(1);
    expect(n.msgs[0]!.title).toContain('review');
    expect(e.driver.reg.size()).toBe(0);
    expect(calls).toBe(3);
  });

  it('frees a run on timeout and ignores a late webhook', async () => {
    const n = new FakeNotifier();
    const e = newEngineFor(await seedRemote(), new FakeGH(), n);
    e.driver.reg.park('acme/api#42', { sessionId: 'run-x', callId: 'c', attempts: 1 }, 3_600_000, (k) =>
      e.driver.onTimeout(k),
    );
    await e.driver.onTimeout('acme/api#42');
    expect(n.msgs).toHaveLength(1);
    expect(n.msgs[0]!.title).toContain('review');
    expect(e.driver.reg.size()).toBe(0);

    await e.resume(checkBody('success', 42)); // benign no-op
    expect(n.msgs).toHaveLength(1);
  });

  it('no-ops on an unknown PR', async () => {
    const n = new FakeNotifier();
    const e = newEngineFor(await seedRemote(), new FakeGH(), n);
    await e.resume(checkBody('success', 99));
    expect(n.msgs).toHaveLength(0);
  });

  it('ignores a check that is not its own', async () => {
    const n = new FakeNotifier();
    const e = newEngineFor(await seedRemote(), new FakeGH(), n);
    const body =
      '{"check_run":{"name":"some-other-check","status":"completed","conclusion":"failure"},"repository":{"full_name":"acme/api"}}';
    await e.resume(body);
    expect(n.msgs).toHaveLength(0);
  });

  it('propagates a triage error on kickoff and parks nothing', async () => {
    const s = spec();
    s.triage = async () => {
      throw new Error('triage boom');
    };
    const e = newEngineFor(await seedRemote('r2'), new FakeGH(), new FakeNotifier(), s);
    await expect(e.kickoff('{"repo":"acme/api","report":"r"}')).rejects.toThrow();
    expect(e.driver.reg.size()).toBe(0);
  });

  it('exposes the label and check name', async () => {
    const e = newEngineFor('x', new FakeGH(), new FakeNotifier());
    expect(e.label()).toBe('automation-agent');
    expect(e.checkName()).toBe('agent-test-verify');
  });
});
