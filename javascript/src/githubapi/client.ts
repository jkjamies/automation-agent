/**
 * Thin wrapper over `@octokit/rest` exposing the narrow operations this service
 * needs: reading recent commits, opening/labeling/finding agent PRs, and reading
 * the agent verify check.
 *
 * Deterministic tooling — no agent imports (an arch test enforces this).
 *
 * Operations return the value and `throw` on error. I/O is async, so every Client
 * method returns a Promise.
 */

import { Octokit } from '@octokit/rest';

/** Bounds every GitHub request so a stalled connection can't hang a long-running poll. */
const HTTP_TIMEOUT_MS = 30_000;

/** Minimal commit projection for digests. */
export interface Commit {
  sha: string;
  message: string;
  author: string;
  url: string;
  /** Authored time, or null when absent. */
  when: Date | null;
}

/** Minimal pull-request projection. */
export interface PR {
  number: number;
  title: string;
  branch: string;
  headSha: string;
  url: string;
  labels: string[];
}

/** Describes a pull request to open. */
export interface PRInput {
  title: string;
  head: string; // source branch
  base: string; // target branch
  body?: string;
}

/** The agent verify check's state for a ref. */
export interface CheckResult {
  found: boolean;
  name: string;
  status: string; // queued | in_progress | completed
  conclusion: string; // success | failure | ... (when completed)
  /** The check's output (lint findings), used to re-triage on resume. */
  outputText: string;
  startedAt: Date | null;
  completedAt: Date | null;
}

/** One file in a base...head comparison. */
export interface ChangedFile {
  path: string;
  status: string; // added | modified | removed | renamed | ...
  additions: number;
  deletions: number;
}

/** Summarizes what changed between two refs (base...head). */
export interface Comparison {
  totalCommits: number;
  files: ChangedFile[];
}

/** The parsed essentials of a GitHub check_run webhook event. */
export interface CheckEvent {
  action: string; // created | completed | rerequested
  checkName: string;
  status: string; // queued | in_progress | completed
  conclusion: string; // success | failure | ... (when completed)
  headSha: string;
  prNumber: number;
  prBranch: string;
  repoFullName: string; // owner/name
  /** The check's output (lint findings), used to re-triage on resume. */
  outputText: string;
}

/**
 * The slice of the Octokit surface this Client uses. Declaring it lets tests
 * inject a fake octokit-like object instead of making network calls. A real
 * {@link Octokit} satisfies this shape.
 */
export interface OctokitLike {
  rest: {
    repos: {
      listCommits(params: {
        owner: string;
        repo: string;
        since: string;
        per_page: number;
      }): Promise<{ data: unknown[] }>;
      getContent(params: {
        owner: string;
        repo: string;
        path: string;
        ref?: string;
      }): Promise<{ data: unknown }>;
      compareCommits(params: {
        owner: string;
        repo: string;
        base: string;
        head: string;
      }): Promise<{ data: unknown }>;
    };
    pulls: {
      create(params: {
        owner: string;
        repo: string;
        title: string;
        head: string;
        base: string;
        body: string;
      }): Promise<{ data: unknown }>;
      list(params: {
        owner: string;
        repo: string;
        state: 'open';
        head?: string;
        per_page: number;
      }): Promise<{ data: unknown[] }>;
    };
    issues: {
      addLabels(params: {
        owner: string;
        repo: string;
        issue_number: number;
        labels: string[];
      }): Promise<unknown>;
    };
    checks: {
      listForRef(params: {
        owner: string;
        repo: string;
        ref: string;
        check_name: string;
        filter: 'latest' | 'all';
      }): Promise<{ data: { total_count: number; check_runs: unknown[] } }>;
    };
  };
  /** Auto-follows pagination, returning the concatenated `data` items. */
  paginate(fn: unknown, params: unknown): Promise<unknown[]>;
}

/**
 * A thin wrapper over an Octokit instance. Owner/repo are passed per call so one
 * client serves many repositories.
 */
export class Client {
  private readonly gh: OctokitLike;

  /**
   * Build a Client. An empty/omitted token yields an unauthenticated client
   * (fine for public reads and tests). A pre-built octokit-like object may be
   * injected directly (used by tests).
   */
  constructor(token = '') {
    // A real Octokit's overloaded method/paginate signatures are narrower than
    // the OctokitLike shape used for test fakes; cast through unknown at this
    // single trusted boundary. A 30s request timeout bounds every call so a stalled
    // GitHub connection can't hang a long-running poll.
    const opts = { request: { timeout: HTTP_TIMEOUT_MS } };
    const oct = token ? new Octokit({ auth: token, ...opts }) : new Octokit(opts);
    this.gh = oct as unknown as OctokitLike;
  }

  /**
   * Build a Client around an injected octokit-like object, bypassing the real
   * Octokit constructor. Lets tests fake the network surface.
   */
  static withOctokit(gh: OctokitLike): Client {
    const c: Client = Object.create(Client.prototype) as Client;
    (c as unknown as { gh: OctokitLike }).gh = gh;
    return c;
  }

  /** Return commits to owner/repo authored since the given time. */
  async listCommitsSince(owner: string, repo: string, since: Date): Promise<Commit[]> {
    try {
      const data = await this.gh.paginate(this.gh.rest.repos.listCommits, {
        owner,
        repo,
        since: since.toISOString(),
        per_page: 100,
      });
      return data.map(toCommit);
    } catch (err) {
      throw new Error(`list commits ${owner}/${repo}: ${errMsg(err)}`);
    }
  }

  /** Return the base...head comparison (commit count + changed files). */
  async compare(owner: string, repo: string, base: string, head: string): Promise<Comparison> {
    try {
      const { data } = await this.gh.rest.repos.compareCommits({ owner, repo, base, head });
      return toComparison(data);
    } catch (err) {
      throw new Error(`compare ${owner}/${repo} ${base}...${head}: ${errMsg(err)}`);
    }
  }

  /** Open a pull request. */
  async createPr(owner: string, repo: string, input: PRInput): Promise<PR> {
    try {
      const { data } = await this.gh.rest.pulls.create({
        owner,
        repo,
        title: input.title,
        head: input.head,
        base: input.base,
        body: input.body ?? '',
      });
      return toPr(data);
    } catch (err) {
      throw new Error(`create PR ${owner}/${repo}: ${errMsg(err)}`);
    }
  }

  /** Add labels to a PR (PRs are issues for the labels API). */
  async addLabels(owner: string, repo: string, number: number, ...labels: string[]): Promise<void> {
    try {
      await this.gh.rest.issues.addLabels({ owner, repo, issue_number: number, labels });
    } catch (err) {
      throw new Error(`add labels to ${owner}/${repo}#${number}: ${errMsg(err)}`);
    }
  }

  /**
   * Return the open PR whose head is the given branch, or null. Lookup is by branch (the
   * GitHub `head=owner:branch` filter), not the agent label — the label is write-only,
   * applied on creation for humans to filter on.
   */
  async findOpenPrByBranch(owner: string, repo: string, branch: string): Promise<PR | null> {
    try {
      const { data } = await this.gh.rest.pulls.list({
        owner,
        repo,
        state: 'open',
        head: `${owner}:${branch}`,
        per_page: 1,
      });
      return data.length > 0 ? toPr(data[0]) : null;
    } catch (err) {
      throw new Error(`list PRs ${owner}/${repo} head ${branch}: ${errMsg(err)}`);
    }
  }

  /**
   * Return the named check's state for ref, or `{ found: false }` if absent.
   */
  async agentCheck(
    owner: string,
    repo: string,
    ref: string,
    checkName: string,
  ): Promise<CheckResult> {
    try {
      const { data } = await this.gh.rest.checks.listForRef({
        owner,
        repo,
        ref,
        check_name: checkName,
        filter: 'latest', // on a re-run, return only the most recent run per check
      });
      // Guard on both the reported count and the actual page contents.
      if (data.total_count === 0 || data.check_runs.length === 0) {
        return notFound();
      }
      const cr = data.check_runs[0] as Record<string, unknown>;
      const out: CheckResult = {
        found: true,
        name: str(cr.name),
        status: str(cr.status),
        conclusion: str(cr.conclusion),
        outputText: '',
        startedAt: toDate(cr.started_at),
        completedAt: toDate(cr.completed_at),
      };
      const output = cr.output as Record<string, unknown> | null | undefined;
      if (output) {
        let text = str(output.text);
        if (text === '') {
          text = str(output.summary);
        }
        out.outputText = text;
      }
      return out;
    } catch (err) {
      throw new Error(`list check runs ${owner}/${repo}@${ref}: ${errMsg(err)}`);
    }
  }

  /**
   * Return the decoded contents of a file at ref (ref may be `""` for the default
   * branch).
   *
   * @throws Error if the path is a directory, the file is missing, or decoding fails.
   */
  async getFileContent(owner: string, repo: string, path: string, ref = ''): Promise<string> {
    let data: unknown;
    try {
      const params = ref ? { owner, repo, path, ref } : { owner, repo, path };
      ({ data } = await this.gh.rest.repos.getContent(params));
    } catch (err) {
      throw new Error(`get ${owner}/${repo}:${path}: ${errMsg(err)}`);
    }
    if (Array.isArray(data)) {
      throw new Error(`${path} is a directory, not a file`);
    }
    const fc = data as Record<string, unknown>;
    if (fc.type !== 'file' || typeof fc.content !== 'string') {
      throw new Error(`${path} is not a file`);
    }
    try {
      const encoding = fc.encoding === 'base64' ? 'base64' : 'utf-8';
      return Buffer.from(fc.content, encoding as BufferEncoding).toString('utf-8');
    } catch (err) {
      throw new Error(`decode ${path}: ${errMsg(err)}`);
    }
  }
}

/**
 * Parse a check_run webhook body into a {@link CheckEvent}.
 *
 * Missing fields degrade to empty/0 defaults.
 *
 * @throws Error on invalid JSON.
 */
export function parseCheckRunEvent(body: string | Uint8Array): CheckEvent {
  let ev: Record<string, unknown>;
  try {
    const text = typeof body === 'string' ? body : Buffer.from(body).toString('utf-8');
    ev = JSON.parse(text) as Record<string, unknown>;
  } catch (err) {
    throw new Error(`parse check_run event: ${errMsg(err)}`);
  }

  const cr = (ev.check_run as Record<string, unknown> | undefined) ?? {};
  const repo = (ev.repository as Record<string, unknown> | undefined) ?? {};
  const out: CheckEvent = {
    action: str(ev.action),
    checkName: str(cr.name),
    status: str(cr.status),
    conclusion: str(cr.conclusion),
    headSha: str(cr.head_sha),
    prNumber: 0,
    prBranch: '',
    repoFullName: str(repo.full_name),
    outputText: '',
  };
  const prs = cr.pull_requests as unknown[] | undefined;
  if (prs && prs.length > 0) {
    const first = (prs[0] as Record<string, unknown> | null) ?? {};
    out.prNumber = num(first.number);
    const head = (first.head as Record<string, unknown> | undefined) ?? {};
    out.prBranch = str(head.ref);
  }
  const output = cr.output as Record<string, unknown> | null | undefined;
  if (output) {
    let text = str(output.text);
    if (text === '') {
      text = str(output.summary);
    }
    out.outputText = text;
  }
  return out;
}

function notFound(): CheckResult {
  return {
    found: false,
    name: '',
    status: '',
    conclusion: '',
    outputText: '',
    startedAt: null,
    completedAt: null,
  };
}

function toCommit(raw: unknown): Commit {
  const rc = raw as Record<string, unknown>;
  const c = (rc.commit as Record<string, unknown> | null | undefined) ?? {};
  const author = (c.author as Record<string, unknown> | null | undefined) ?? {};
  return {
    sha: str(rc.sha),
    message: str(c.message),
    author: str(author.name),
    url: str(rc.html_url),
    when: toDate(author.date),
  };
}

function toPr(raw: unknown): PR {
  const pr = raw as Record<string, unknown>;
  const head = (pr.head as Record<string, unknown> | null | undefined) ?? {};
  const labelsRaw = (pr.labels as unknown[] | null | undefined) ?? [];
  const labels = labelsRaw.map((l) => str((l as Record<string, unknown>).name));
  return {
    number: num(pr.number),
    title: str(pr.title),
    branch: str(head.ref),
    headSha: str(head.sha),
    url: str(pr.html_url),
    labels,
  };
}

function toComparison(raw: unknown): Comparison {
  const c = raw as Record<string, unknown>;
  const filesRaw = (c.files as unknown[] | null | undefined) ?? [];
  const files: ChangedFile[] = filesRaw.map((f) => {
    const file = f as Record<string, unknown>;
    return {
      path: str(file.filename),
      status: str(file.status),
      additions: num(file.additions),
      deletions: num(file.deletions),
    };
  });
  return { totalCommits: num(c.total_commits), files };
}

/** Coerce a possibly-missing string field to `""`. */
function str(v: unknown): string {
  return typeof v === 'string' ? v : '';
}

/** Coerce a possibly-missing number field to `0`. */
function num(v: unknown): number {
  return typeof v === 'number' ? v : 0;
}

/** Parse an ISO-8601 timestamp to a Date, or null when absent/empty. */
function toDate(v: unknown): Date | null {
  if (typeof v !== 'string' || v === '') {
    return null;
  }
  const d = new Date(v);
  return Number.isNaN(d.getTime()) ? null : d;
}

/** Extract a message from a thrown value. */
function errMsg(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}
