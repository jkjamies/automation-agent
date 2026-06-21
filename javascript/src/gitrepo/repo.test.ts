// Tests for the git working-tree operations.
//
// Uses a LOCAL seed repo (no network). A local "remote" is seeded with one commit, then
// cloned, and clone/checkout/commitAll/push/head/checkoutRemote are exercised against it.
import { mkdtemp, rm, writeFile } from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { simpleGit } from 'simple-git';
import { afterAll, beforeAll, describe, expect, it } from 'vitest';
import { Author, authUrl, NoChangesError, Repo } from './repo';

let tmpRoot: string;

beforeAll(async () => {
  tmpRoot = await mkdtemp(path.join(os.tmpdir(), 'gitrepo-test-'));
});

afterAll(async () => {
  await rm(tmpRoot, { recursive: true, force: true });
});

let seedCounter = 0;

/** Create a local repo with one commit to act as the clone source. */
async function seedRemote(): Promise<string> {
  const dir = path.join(tmpRoot, `remote-${seedCounter++}`);
  const git = simpleGit();
  await git.init([dir]);
  const repo = simpleGit(dir);
  await repo.addConfig('user.name', 'seed');
  await repo.addConfig('user.email', 's@x');
  // Push targets a checked-out branch; allow it so push() succeeds against a
  // non-bare local remote.
  await repo.addConfig('receive.denyCurrentBranch', 'ignore');
  await writeFile(path.join(dir, 'README.md'), 'hi');
  await repo.add(['README.md']);
  await repo.commit('init');
  return dir;
}

/** Allocate a fresh work-tree path under the temp root (must not pre-exist). */
function workDir(name: string): string {
  return path.join(tmpRoot, `work-${name}-${seedCounter++}`);
}

describe('gitrepo', () => {
  it('clones, branches, commits and pushes', async () => {
    const remote = await seedRemote();
    const work = workDir('cbcp');

    const r = await Repo.clone(remote, work, '');

    await r.checkout('agent/fix', true);
    await writeFile(r.path('fix.txt'), 'patched');

    const author: Author = { name: 'agent', email: 'a@x' };
    const sha = await r.commitAll('apply fix', author);
    const head = await r.head();
    expect(head).toBe(sha);

    expect(r.dir()).toBe(work);

    await r.push();
    // A second push with no new commits is up-to-date, not an error.
    await r.push();

    // The remote should now have the pushed branch at the committed SHA.
    const rr = simpleGit(remote);
    const ref = (await rr.revparse(['agent/fix'])).trim();
    expect(ref).toBe(sha);
  });

  it('checks out an existing remote branch', async () => {
    const remote = await seedRemote();

    // First clone: create and push a branch.
    const r1 = await Repo.clone(remote, workDir('cr1'), '');
    await r1.checkout('feature', true);
    await writeFile(r1.path('f.txt'), 'x');
    const sha = await r1.commitAll('feat', { name: 'a', email: 'a@x' });
    await r1.push();

    // Second clone: check out the existing remote branch.
    const r2 = await Repo.clone(remote, workDir('cr2'), '');
    await r2.checkoutRemote('feature');
    expect(await r2.head()).toBe(sha);

    await expect(r2.checkoutRemote('does-not-exist')).rejects.toThrow();
  });

  it('errors checking out a missing branch', async () => {
    const r = await Repo.clone(await seedRemote(), workDir('miss'), '');
    await expect(r.checkout('does-not-exist', false)).rejects.toThrow();
  });

  it('raises NoChangesError committing a clean tree', async () => {
    const r = await Repo.clone(await seedRemote(), workDir('clean'), '');
    await expect(r.commitAll('nothing changed', { name: 'a', email: 'a@x' })).rejects.toThrow(
      NoChangesError,
    );
  });

  it('errors cloning a nonexistent source', async () => {
    const work = workDir('nope');
    await expect(Repo.clone(path.join(tmpRoot, 'does-not-exist'), work, '')).rejects.toThrow();
  });

  it('embeds the token into https URLs only', () => {
    // token embeds x-access-token for https; local paths untouched.
    expect(authUrl('https://github.com/o/r.git', 'tok')).toBe(
      'https://x-access-token:tok@github.com/o/r.git',
    );
    const local = path.join(tmpRoot, 'repo');
    expect(authUrl(local, 'tok')).toBe(local);
    expect(authUrl('https://github.com/o/r.git', '')).toBe('https://github.com/o/r.git');
  });
});
