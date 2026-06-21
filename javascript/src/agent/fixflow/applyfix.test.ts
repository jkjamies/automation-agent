// Tests for apply-fix: a local seed git repo as the clone source, a fake GitHub
// capturing the created PR + labels, and the branch/commit/push/ensure-PR behavior.
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { simpleGit } from 'simple-git';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import type { PR, PRInput } from '../../githubapi/client';
import { type ApplyConfig, applyFix, type FileEdit, type GitHub } from './applyfix';

class FakeGH implements GitHub {
  created: PRInput | null = null;
  labeled: string[] = [];
  constructor(
    private readonly existing: PR[] = [],
    private readonly createErr?: Error,
  ) {}
  async findAgentPrs(): Promise<PR[]> {
    return this.existing;
  }
  async createPr(_o: string, _r: string, input: PRInput): Promise<PR> {
    if (this.createErr) throw this.createErr;
    this.created = input;
    return { number: 42, title: input.title, branch: input.head, headSha: '', url: 'https://gh/pr/42', labels: [] };
  }
  async addLabels(_o: string, _r: string, _n: number, ...labels: string[]): Promise<void> {
    this.labeled.push(...labels);
  }
}

let root: string;
beforeEach(() => {
  root = mkdtempSync(join(tmpdir(), 'ff-apply-'));
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

function applyCfg(remote: string): ApplyConfig {
  return {
    owner: 'acme',
    repo: 'api',
    cloneUrl: remote,
    token: '',
    base: 'main',
    branch: 'agent/fix',
    newBranch: true,
    label: 'automation-agent',
    commitMessage: 'fix',
    prTitle: 'Fix',
    prBody: 'auto',
    author: { name: 'agent', email: 'a@x' },
  };
}

describe('applyFix', () => {
  it('creates a labeled PR and pushes the branch', async () => {
    const remote = await seedRemote();
    const gh = new FakeGH();
    const res = await applyFix(gh, applyCfg(remote), [
      { path: 'src/foo.ts', content: 'export const foo = 1;\n' },
    ]);
    expect(res.pr.number).toBe(42);
    expect(res.headSha).not.toBe('');
    expect(gh.created?.head).toBe('agent/fix');
    expect(gh.labeled).toEqual(['automation-agent']);

    const branches = await simpleGit(remote).branch();
    expect(branches.all).toContain('agent/fix');
  });

  it('reuses an existing branch on retry', async () => {
    const remote = await seedRemote();
    await applyFix(new FakeGH(), applyCfg(remote), [{ path: 'a.ts', content: 'export const a = 1;\n' }]);

    const retry = applyCfg(remote);
    retry.newBranch = false;
    const gh = new FakeGH([{ number: 9, title: '', branch: 'agent/fix', headSha: '', url: '', labels: [] }]);
    const res = await applyFix(gh, retry, [{ path: 'b.ts', content: 'export const b = 2;\n' }]);
    expect(res.pr.number).toBe(9);
    expect(gh.created).toBeNull(); // reused, did not create
  });

  it('throws with no edits', async () => {
    await expect(applyFix(new FakeGH(), applyCfg('x'), [] as FileEdit[])).rejects.toThrow();
  });

  it('throws on a clone error', async () => {
    const bad = applyCfg(join(root, 'nope'));
    await expect(
      applyFix(new FakeGH(), bad, [{ path: 'x.ts', content: 'export {};\n' }]),
    ).rejects.toThrow();
  });

  it('propagates a create-PR error', async () => {
    const remote = await seedRemote();
    const gh = new FakeGH([], new Error('boom'));
    await expect(
      applyFix(gh, applyCfg(remote), [{ path: 'x.ts', content: 'export {};\n' }]),
    ).rejects.toThrow('boom');
  });

  it('rejects a path that escapes the checkout', async () => {
    const remote = await seedRemote();
    await expect(
      applyFix(new FakeGH(), applyCfg(remote), [{ path: '../escape.ts', content: 'x' }]),
    ).rejects.toThrow();
  });
});
