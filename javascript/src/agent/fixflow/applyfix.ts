/**
 * The apply mechanics: checkout, write edits, commit, push, ensure a labeled PR.
 *
 * {@link openRepo} clones into a fresh temp dir and checks out the agent branch (created
 * from base on kickoff, the existing remote branch on retry). {@link commit} writes the
 * edits path-safely, commits, pushes, and ensures a labeled PR exists. {@link applyFix}
 * does both in one step (a test convenience).
 */
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { dirname } from 'node:path';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import type { Comparison, PR, PRInput } from '../../githubapi/client';
import { type Author, Repo } from '../../gitrepo/repo';
import { safeJoin } from './files';

/** The slice of githubapi the apply step + terminal summary need (consumer-defined, fakeable). */
export interface GitHub {
  findAgentPrs(owner: string, repo: string, label: string): Promise<PR[]>;
  createPr(owner: string, repo: string, input: PRInput): Promise<PR>;
  addLabels(owner: string, repo: string, num: number, ...labels: string[]): Promise<void>;
  compare(owner: string, repo: string, base: string, head: string): Promise<Comparison>;
}

/** A whole-file write an analyze step produces (a rewritten source file, a generated test, …). */
export interface FileEdit {
  path: string; // repo-relative path
  content: string;
}

/** Parameterizes one apply. */
export interface ApplyConfig {
  owner: string;
  repo: string;
  cloneUrl: string;
  token: string;
  base: string; // base branch the PR targets
  branch: string; // agent working branch
  newBranch: boolean; // true on kickoff (create from base); false on retry (reuse)
  label: string;
  commitMessage: string;
  prTitle: string;
  prBody: string;
  author: Author;
}

/** The outcome of one apply. */
export interface ApplyResult {
  pr: PR;
  headSha: string;
}

/**
 * Clone the repo into a fresh temp dir and check out the agent branch. The caller must
 * remove `repo.dir()` when done.
 */
export async function openRepo(cfg: ApplyConfig): Promise<Repo> {
  // Clone into a fresh, empty temp dir; git clone accepts an existing empty
  // directory, so there is no create-then-delete race on the target path.
  const dir = mkdtempSync(join(tmpdir(), 'agentfix-'));
  let repo: Repo;
  try {
    repo = await Repo.clone(cfg.cloneUrl, dir, cfg.token);
  } catch (err) {
    rmSync(dir, { recursive: true, force: true });
    throw err;
  }
  try {
    if (cfg.newBranch) {
      await repo.checkout(cfg.branch, true);
    } else {
      await repo.checkoutRemote(cfg.branch);
    }
  } catch (err) {
    rmSync(repo.dir(), { recursive: true, force: true });
    throw err;
  }
  return repo;
}

/** Write edits into the working tree, commit, push, and ensure a labeled PR exists. */
export async function commit(
  gh: GitHub,
  repo: Repo,
  cfg: ApplyConfig,
  edits: FileEdit[],
): Promise<ApplyResult> {
  if (edits.length === 0) {
    throw new Error('apply: no edits to apply');
  }
  writeEdits(repo, edits);
  const sha = await repo.commitAll(cfg.commitMessage, cfg.author);
  await repo.push();
  const pr = await ensurePr(gh, cfg);
  return { pr, headSha: sha };
}

/**
 * Open a checkout and commit edits in one step — a convenience used in tests; the engine
 * interleaves analysis between {@link openRepo} and {@link commit}.
 */
export async function applyFix(
  gh: GitHub,
  cfg: ApplyConfig,
  edits: FileEdit[],
): Promise<ApplyResult> {
  const repo = await openRepo(cfg);
  try {
    return await commit(gh, repo, cfg, edits);
  } finally {
    rmSync(repo.dir(), { recursive: true, force: true });
  }
}

function writeEdits(repo: Repo, edits: FileEdit[]): void {
  for (const e of edits) {
    // Reject LLM-controlled paths that escape the checkout (path traversal).
    const full = safeJoin(repo.dir(), e.path);
    mkdirSync(dirname(full), { recursive: true });
    writeFileSync(full, e.content, 'utf-8');
  }
}

async function ensurePr(gh: GitHub, cfg: ApplyConfig): Promise<PR> {
  const existing = await gh.findAgentPrs(cfg.owner, cfg.repo, cfg.label);
  for (const pr of existing) {
    if (pr.branch === cfg.branch) {
      return pr;
    }
  }
  const pr = await gh.createPr(cfg.owner, cfg.repo, {
    title: cfg.prTitle,
    head: cfg.branch,
    base: cfg.base,
    body: cfg.prBody,
  });
  await gh.addLabels(cfg.owner, cfg.repo, pr.number, cfg.label);
  pr.labels = [...pr.labels, cfg.label];
  return pr;
}
