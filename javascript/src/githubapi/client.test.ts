/**
 * Tests for the GitHub API client. We inject an octokit-like fake via
 * `Client.withOctokit`, exercising the real logic paths (pagination, label filter,
 * attempt count, check projection, not-found, file decode/directory) with NO network.
 * `parseCheckRunEvent` is pure and tested directly.
 */

import { describe, it, expect } from 'vitest';
import { Client, parseCheckRunEvent, type OctokitLike } from './client';

// --- octokit-shaped fake -----------------------------------------------------

interface FakeOpts {
  commits?: unknown[];
  newPull?: unknown;
  pulls?: unknown[];
  prCommits?: unknown[];
  checkRunsByRef?: Record<string, unknown[]>;
  contents?: unknown;
}

interface Seen {
  createParams?: Record<string, unknown>;
  labeled?: { number: number; labels: string[] };
  pullsState?: string;
  checkNameSeen?: string;
  contentsRef?: string | undefined;
}

function makeClient(opts: FakeOpts): { client: Client; seen: Seen } {
  const seen: Seen = {};

  // The real Octokit.paginate(fn, params) dispatches by the passed method
  // reference; our fake instead reads from `opts` keyed by which method was
  // handed in (we tag each fake method with a `_kind`).
  const repos = {
    listCommits: tag('commits', async () => ({ data: opts.commits ?? [] })),
    getContent: async (params: { ref?: string }) => {
      seen.contentsRef = params.ref;
      return { data: opts.contents };
    },
  };
  const pulls = {
    create: async (params: Record<string, unknown>) => {
      seen.createParams = params;
      return { data: opts.newPull };
    },
    list: tag('pulls', async (params: { state: string }) => {
      seen.pullsState = params.state;
      return { data: opts.pulls ?? [] };
    }),
    listCommits: tag('prCommits', async () => ({ data: opts.prCommits ?? [] })),
  };
  const issues = {
    addLabels: async (params: { issue_number: number; labels: string[] }) => {
      seen.labeled = { number: params.issue_number, labels: params.labels };
      return {};
    },
  };
  const checks = {
    listForRef: async (params: { ref: string; check_name: string }) => {
      seen.checkNameSeen = params.check_name;
      const runs = opts.checkRunsByRef?.[params.ref] ?? [];
      return { data: { total_count: runs.length, check_runs: runs } };
    },
  };

  const gh: OctokitLike = {
    rest: { repos, pulls, issues, checks } as unknown as OctokitLike['rest'],
    // Resolve the tagged method, invoke it, and return its `.data` (mirrors
    // octokit's auto-pagination over a single page).
    paginate: async (fn: unknown, params: unknown) => {
      const res = await (fn as (p: unknown) => Promise<{ data: unknown[] }>)(params);
      return res.data;
    },
  };

  return { client: Client.withOctokit(gh), seen };
}

/** Attach a tag so a no-op (kept for symmetry with octokit's dispatch). */
function tag<T extends (...args: never[]) => unknown>(kind: string, fn: T): T {
  (fn as unknown as { _kind: string })._kind = kind;
  return fn;
}

// --- fixture builders --------------------------------------------------------

function commit(
  sha: string,
  message: string,
  authorName: string,
  when: string | null,
  htmlUrl: string,
): unknown {
  return {
    sha,
    html_url: htmlUrl,
    commit: { message, author: { name: authorName, date: when } },
  };
}

function pull(
  number: number,
  title: string,
  ref: string,
  sha: string,
  htmlUrl: string,
  labels: string[],
): unknown {
  return {
    number,
    title,
    head: { ref, sha },
    html_url: htmlUrl,
    labels: labels.map((name) => ({ name })),
  };
}

function checkRun(
  name: string,
  status: string,
  conclusion: string,
  startedAt: string | null,
  completedAt: string | null,
  text: string | null = null,
  summary: string | null = null,
): unknown {
  return {
    name,
    status,
    conclusion,
    started_at: startedAt,
    completed_at: completedAt,
    output: { text, summary },
  };
}

// --- listCommitsSince --------------------------------------------------------

describe('listCommitsSince', () => {
  it('projects commits', async () => {
    const when = '2026-06-19T10:00:00Z';
    const { client } = makeClient({
      commits: [commit('abc', 'fix bug', 'Jane', when, 'https://gh/abc')],
    });

    const commits = await client.listCommitsSince('o', 'r', new Date(0));

    expect(commits).toHaveLength(1);
    const got = commits[0]!;
    expect(got.sha).toBe('abc');
    expect(got.author).toBe('Jane');
    expect(got.message).toBe('fix bug');
    expect(got.url).toBe('https://gh/abc');
    expect(got.when?.toISOString()).toBe('2026-06-19T10:00:00.000Z');
  });
});

// --- createPr + addLabels ----------------------------------------------------

describe('createPr and addLabels', () => {
  it('opens and labels a PR', async () => {
    const { client, seen } = makeClient({
      newPull: pull(5, 'fix lint', 'agent/fix', 'deadbeef', 'https://gh/pr/5', []),
    });

    const pr = await client.createPr('o', 'r', {
      title: 'fix lint',
      head: 'agent/fix',
      base: 'main',
    });
    expect(pr.number).toBe(5);
    expect(pr.branch).toBe('agent/fix');
    expect(pr.headSha).toBe('deadbeef');
    expect(pr.url).toBe('https://gh/pr/5');
    expect(seen.createParams?.title).toBe('fix lint');
    expect(seen.createParams?.base).toBe('main');
    expect(seen.createParams?.body).toBe('');

    await client.addLabels('o', 'r', 5, 'automation-agent');
    expect(seen.labeled).toEqual({ number: 5, labels: ['automation-agent'] });
  });
});

// --- findAgentPrs ------------------------------------------------------------

describe('findAgentPrs', () => {
  it('returns only PRs carrying the label', async () => {
    const { client, seen } = makeClient({
      pulls: [
        pull(5, '', 'agent/fix', 's5', '', ['automation-agent']),
        pull(6, '', 'feature', 's6', '', ['enhancement']),
      ],
    });

    const prs = await client.findAgentPrs('o', 'r', 'automation-agent');
    expect(seen.pullsState).toBe('open');
    expect(prs).toHaveLength(1);
    expect(prs[0]!.number).toBe(5);
  });
});

// --- attemptCount ------------------------------------------------------------

describe('attemptCount', () => {
  it('counts PR commits', async () => {
    const { client } = makeClient({
      prCommits: [commit('a', '', '', null, ''), commit('b', '', '', null, '')],
    });

    const n = await client.attemptCount('o', 'r', 7);
    expect(n).toBe(2);
  });
});

// --- agentCheck --------------------------------------------------------------

describe('agentCheck', () => {
  it('found: empty text falls back to summary', async () => {
    const completed = '2026-06-19T11:00:00Z';
    const { client, seen } = makeClient({
      checkRunsByRef: {
        sha1: [
          checkRun('agent-lint-verify', 'completed', 'success', null, completed, '', 'all checks passed'),
        ],
      },
    });

    const res = await client.agentCheck('o', 'r', 'sha1', 'agent-lint-verify');
    expect(res.found).toBe(true);
    expect(res.status).toBe('completed');
    expect(res.conclusion).toBe('success');
    expect(res.outputText).toBe('all checks passed');
    expect(res.completedAt?.toISOString()).toBe('2026-06-19T11:00:00.000Z');
    expect(seen.checkNameSeen).toBe('agent-lint-verify');
  });

  it('prefers text over summary', async () => {
    const { client } = makeClient({
      checkRunsByRef: {
        sha1: [
          checkRun(
            'agent-lint-verify',
            'completed',
            'failure',
            null,
            null,
            'errcheck: unchecked error',
            'ignored',
          ),
        ],
      },
    });
    const res = await client.agentCheck('o', 'r', 'sha1', 'agent-lint-verify');
    expect(res.outputText).toBe('errcheck: unchecked error');
  });

  it('missing: returns found=false', async () => {
    const { client } = makeClient({ checkRunsByRef: { sha2: [] } });

    const missing = await client.agentCheck('o', 'r', 'sha2', 'agent-lint-verify');
    expect(missing.found).toBe(false);
    expect(missing.name).toBe('');
  });
});

// --- getFileContent ----------------------------------------------------------

describe('getFileContent', () => {
  it('decodes base64 content at a ref', async () => {
    const encoded = Buffer.from('package foo\n', 'utf-8').toString('base64');
    const { client, seen } = makeClient({
      contents: { type: 'file', encoding: 'base64', path: 'internal/foo.go', content: encoded },
    });

    const got = await client.getFileContent('o', 'r', 'internal/foo.go', 'main');
    expect(got).toBe('package foo\n');
    expect(seen.contentsRef).toBe('main');
  });

  it('default ref omits the ref param', async () => {
    const encoded = Buffer.from('hello\n', 'utf-8').toString('base64');
    const { client, seen } = makeClient({
      contents: { type: 'file', encoding: 'base64', content: encoded },
    });
    const got = await client.getFileContent('o', 'r', 'x', '');
    expect(got).toBe('hello\n');
    expect(seen.contentsRef).toBeUndefined();
  });

  it('throws on a directory', async () => {
    const { client } = makeClient({ contents: [{}, {}] });
    await expect(client.getFileContent('o', 'r', 'internal', '')).rejects.toThrow(
      /is a directory/,
    );
  });
});

// --- error wrapping ----------------------------------------------------------

describe('error wrapping', () => {
  it('wraps method errors with context', async () => {
    const gh: OctokitLike = {
      // The method reference must exist (it is passed into paginate); the failure
      // is raised by paginate itself, the same call path the real client uses.
      rest: { repos: { listCommits: () => undefined } } as unknown as OctokitLike['rest'],
      paginate: async () => {
        throw new Error('network down');
      },
    };
    const client = Client.withOctokit(gh);
    await expect(client.listCommitsSince('o', 'r', new Date())).rejects.toThrow(
      /list commits o\/r: network down/,
    );
  });
});

// --- parseCheckRunEvent (pure) ----------------------------------------------

describe('parseCheckRunEvent', () => {
  it('parses a full event', () => {
    const body = `{
      "action":"completed",
      "check_run":{
        "name":"agent-lint-verify",
        "status":"completed",
        "conclusion":"failure",
        "head_sha":"sha123",
        "output":{"text":"errcheck: unchecked error"},
        "pull_requests":[{"number":12,"head":{"ref":"agent/fix"}}]
      },
      "repository":{"full_name":"acme/api"}
    }`;
    const ev = parseCheckRunEvent(body);
    expect(ev.action).toBe('completed');
    expect(ev.checkName).toBe('agent-lint-verify');
    expect(ev.conclusion).toBe('failure');
    expect(ev.headSha).toBe('sha123');
    expect(ev.prNumber).toBe(12);
    expect(ev.prBranch).toBe('agent/fix');
    expect(ev.repoFullName).toBe('acme/api');
    expect(ev.outputText).toBe('errcheck: unchecked error');
  });

  it('falls back to summary when text is empty', () => {
    const ev = parseCheckRunEvent('{"check_run":{"output":{"summary":"all good"}}}');
    expect(ev.outputText).toBe('all good');
  });

  it('degrades missing fields to empty/0 defaults', () => {
    const ev = parseCheckRunEvent('{}');
    expect(ev.action).toBe('');
    expect(ev.checkName).toBe('');
    expect(ev.prNumber).toBe(0);
    expect(ev.prBranch).toBe('');
    expect(ev.repoFullName).toBe('');
    expect(ev.outputText).toBe('');
  });

  it('accepts a Uint8Array body', () => {
    const ev = parseCheckRunEvent(new TextEncoder().encode('{"action":"created"}'));
    expect(ev.action).toBe('created');
  });

  it('throws on invalid JSON', () => {
    expect(() => parseCheckRunEvent('not json')).toThrow(/parse check_run event/);
  });
});
