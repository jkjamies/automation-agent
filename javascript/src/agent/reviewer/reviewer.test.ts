// Deterministic tests for the reviewer engine: the intake decision matrix, the kickoff paths,
// coalesce-to-latest staleness, and the model-calling review pipeline (canned findings). A fake
// GitHub client captures the reads; a scripted FakeLlm returns canned JSON. We never assert on real
// LLM output — only orchestration and deterministic logic.
import { describe, expect, it } from 'vitest';

import type {
  CheckResult,
  CheckRunInput,
  PRFile,
  PullRequestEvent,
  ReviewCommentRef,
  ReviewInput,
  TreeEntry,
} from '../../githubapi/client';
import { FakeLlm } from '../../testutil/fakes';
import {
  type Deps,
  type GitHubClient,
  type Logger,
  Engine,
  isDependencyBot,
  newEngine,
  splitFullName,
} from './reviewer';
import { DecisionKind } from './reviewer';
import { Tier } from './categories';
import { formatDiff, maxBacktickRun, modelForTier, runReview } from './review';
import { Level } from './scorecard';
import { discoverStandards } from './standards';

interface FakeOpts {
  listError?: Error;
  headSha?: string;
  headShaError?: Error;
  existingComments?: ReviewCommentRef[];
  agentCheckFound?: boolean;
  minimizeError?: Error;
  treeEntries?: TreeEntry[];
  treeTruncated?: boolean;
  fileContents?: Record<string, string>;
}

/**
 * A stub GitHub client: returns canned files/head SHA and captures the publish-stage REST writes
 * (reviews, checks, marker upserts, minimizes) so tests assert orchestration, never LLM output.
 */
class FakeGH implements GitHubClient {
  calls = 0;
  readonly reviews: ReviewInput[] = [];
  readonly checkRuns: CheckRunInput[] = [];
  readonly upserts: Array<{ marker: string; body: string }> = [];
  readonly minimized: string[] = [];

  constructor(
    private readonly files: PRFile[] = [],
    private readonly opts: FakeOpts = {},
  ) {}

  async listPRFiles(): Promise<PRFile[]> {
    this.calls += 1;
    if (this.opts.listError) {
      throw this.opts.listError;
    }
    return this.files;
  }

  async pullRequestHeadSha(): Promise<string> {
    if (this.opts.headShaError) {
      throw this.opts.headShaError;
    }
    return this.opts.headSha ?? '';
  }

  createReview(_o: string, _r: string, _n: number, input: ReviewInput): Promise<void> {
    this.reviews.push(input);
    return Promise.resolve();
  }

  upsertMarkerComment(_o: string, _r: string, _n: number, marker: string, body: string): Promise<void> {
    this.upserts.push({ marker, body });
    return Promise.resolve();
  }

  createCheckRun(_o: string, _r: string, input: CheckRunInput): Promise<void> {
    this.checkRuns.push(input);
    return Promise.resolve();
  }

  listReviewComments(): Promise<ReviewCommentRef[]> {
    return Promise.resolve(this.opts.existingComments ?? []);
  }

  minimizeComment(subjectId: string): Promise<void> {
    if (this.opts.minimizeError) {
      return Promise.reject(this.opts.minimizeError);
    }
    this.minimized.push(subjectId);
    return Promise.resolve();
  }

  agentCheck(): Promise<CheckResult> {
    return Promise.resolve({
      found: this.opts.agentCheckFound ?? false,
      name: '',
      status: '',
      conclusion: '',
      outputText: '',
      startedAt: null,
      completedAt: null,
    });
  }

  tree(): Promise<{ entries: TreeEntry[]; truncated: boolean }> {
    return Promise.resolve({
      entries: this.opts.treeEntries ?? [],
      truncated: this.opts.treeTruncated ?? false,
    });
  }

  getFileContent(_o: string, _r: string, path: string): Promise<string> {
    return Promise.resolve(this.opts.fileContents?.[path] ?? '');
  }
}

/** A logger that records the info messages it saw, for asserting which branch ran. */
class CaptureLog implements Logger {
  readonly infos: string[] = [];
  debug(): void {}
  info(msg: string): void {
    this.infos.push(msg);
  }
  warn(): void {}
}

function prFile(partial: Partial<PRFile>): PRFile {
  return { path: '', previousPath: '', status: '', additions: 0, deletions: 0, patch: '', ...partial };
}

function engine(gh: GitHubClient, canned = '[]', overrides: Partial<Deps> = {}): Engine {
  const llm = new FakeLlm(canned);
  return newEngine({
    enabled: true,
    gh,
    baseLlm: llm,
    codeLlm: llm,
    minConfidence: 0.6,
    skipDrafts: true,
    excludeGlobs: ['go.sum', 'vendor/**'],
    maxFiles: 50,
    maxDiffBytes: 1000,
    ...overrides,
  });
}

function event(action: string, kw: Partial<PullRequestEvent> = {}): PullRequestEvent {
  return {
    action,
    number: 1,
    repoFullName: 'o/r',
    headRef: 'feature/x',
    headSha: '',
    baseRef: 'main',
    draft: false,
    labels: [],
    authorLogin: '',
    ...kw,
  };
}

describe('decide matrix', () => {
  const real = [prFile({ path: 'main.go', patch: 'abc' })];

  it('skips an untriggered action before any fetch', async () => {
    const gh = new FakeGH(real);
    expect((await engine(gh).decide(event('closed'))).kind).toBe(DecisionKind.Skip);
    expect(gh.calls).toBe(0);
  });

  it('skips a draft before fetch, but reviews on ready_for_review', async () => {
    const gh = new FakeGH(real);
    expect((await engine(gh).decide(event('opened', { draft: true }))).kind).toBe(DecisionKind.Skip);
    expect(gh.calls).toBe(0);
    expect((await engine(new FakeGH(real)).decide(event('ready_for_review', { draft: true }))).kind).toBe(
      DecisionKind.Review,
    );
  });

  it('skips the agent own branch, the skip-review label, and dependency bots', async () => {
    expect((await engine(new FakeGH(real)).decide(event('opened', { headRef: 'automation-agent/lint' }))).kind).toBe(
      DecisionKind.Skip,
    );
    expect((await engine(new FakeGH(real)).decide(event('opened', { labels: ['skip-review'] }))).kind).toBe(
      DecisionKind.Skip,
    );
    expect((await engine(new FakeGH(real)).decide(event('opened', { authorLogin: 'dependabot[bot]' }))).kind).toBe(
      DecisionKind.Skip,
    );
  });

  it('skips when every file is excluded, after fetch', async () => {
    const gh = new FakeGH([prFile({ path: 'go.sum', patch: 'x' }), prFile({ path: 'vendor/y.go', patch: 'x' })]);
    const d = await engine(gh).decide(event('opened'));
    expect(d.kind).toBe(DecisionKind.Skip);
    expect(gh.calls).toBe(1);
  });

  it('reviews on the filtered size and denies when oversize', async () => {
    const gh = new FakeGH([prFile({ path: 'main.go', patch: '12345' }), prFile({ path: 'go.sum', patch: 'ignored' })]);
    const d = await engine(gh).decide(event('synchronize'));
    expect(d.kind).toBe(DecisionKind.Review);
    expect(d.files).toHaveLength(1);
    expect(d.diffBytes).toBe(5);

    const gh2 = new FakeGH([prFile({ path: 'a.go', patch: 'x' }), prFile({ path: 'b.go', patch: 'x' })]);
    const deny = await engine(gh2, '[]', { maxFiles: 1 }).decide(event('opened'));
    expect(deny.kind).toBe(DecisionKind.Deny);
    expect(deny.reason).not.toBe('');
  });

  it('throws on a malformed repo name and on a list error', async () => {
    await expect(engine(new FakeGH([prFile({ path: 'main.go', patch: 'x' })])).decide(event('opened', { repoFullName: 'noslash' }))).rejects.toThrow();
    await expect(engine(new FakeGH([], { listError: new Error('boom') })).decide(event('opened'))).rejects.toThrow(/list PR files/);
  });
});

describe('split + bot helpers', () => {
  it('splits a valid full name and rejects the rest', () => {
    expect(splitFullName('o/r')).toEqual({ owner: 'o', repo: 'r', ok: true });
    for (const bad of ['noslash', 'a/b/c', '/r', 'o/']) {
      expect(splitFullName(bad).ok).toBe(false);
    }
  });

  it('recognizes the dependency bots', () => {
    expect(isDependencyBot('dependabot[bot]')).toBe(true);
    expect(isDependencyBot('renovate[bot]')).toBe(true);
    expect(isDependencyBot('alice')).toBe(false);
  });
});

describe('kickoff', () => {
  it('no-ops when disabled', async () => {
    const gh = new FakeGH();
    await newEngine({ enabled: false, gh }).kickoff(Buffer.from('not even json'));
    expect(gh.calls).toBe(0);
  });

  it('errors when enabled without a client', async () => {
    const e = newEngine({ enabled: true, gh: null });
    const body = '{"action":"opened","pull_request":{"number":1,"head":{"ref":"x"}},"repository":{"full_name":"o/r"}}';
    await expect(e.kickoff(body)).rejects.toThrow();
  });

  it('errors on a malformed body', async () => {
    await expect(engine(new FakeGH()).kickoff('{bad')).rejects.toThrow();
  });

  it('runs the review path to a scorecard', async () => {
    const canned = '[{"file":"main.go","line":1,"dimension":"performance","severity":"medium","message":"slow","confidence":0.9}]';
    const gh = new FakeGH([prFile({ path: 'main.go', patch: '@@ -1 +1 @@\n+x', status: 'modified' })]);
    const log = new CaptureLog();
    const e = engine(gh, canned, { log });
    const body = '{"action":"opened","pull_request":{"number":7,"head":{"ref":"feature/x"},"base":{"ref":"main"}},"repository":{"full_name":"o/r"}}';
    await e.kickoff(body);
    expect(gh.calls).toBe(1);
    expect(log.infos).toContain('review published');
    // Publish stage: the in-diff finding posts inline, the summary comment upserts, and exactly one
    // advisory check is created — success|neutral, never failure.
    expect(gh.reviews).toHaveLength(1);
    expect(gh.reviews[0]!.comments[0]!.body).toContain('<!-- ar-fp:');
    expect(gh.upserts).toHaveLength(1);
    expect(gh.upserts[0]!.marker).toBe('<!-- automation-agent:review:o/r#7 -->');
    expect(gh.checkRuns).toHaveLength(1);
    expect(gh.checkRuns[0]!.name).toBe('agent-review');
    expect(['success', 'neutral']).toContain(gh.checkRuns[0]!.conclusion);
  });

  it('skips re-posting when the head SHA was already published', async () => {
    const canned = '[{"file":"main.go","line":1,"dimension":"performance","severity":"medium","message":"slow","confidence":0.9}]';
    const gh = new FakeGH([prFile({ path: 'main.go', patch: '@@\n+x', status: 'modified' })], {
      agentCheckFound: true,
    });
    const body = '{"action":"opened","pull_request":{"number":7,"head":{"ref":"feature/x","sha":"s"},"base":{"ref":"main"}},"repository":{"full_name":"o/r"}}';
    await engine(gh, canned).kickoff(body);
    expect(gh.checkRuns).toHaveLength(0); // already-published guard short-circuits the writes
    expect(gh.upserts).toHaveLength(0);
  });

  it('skips a stale review superseded by a newer push, proceeds on a lookup error', async () => {
    const body = (sha: string): string =>
      `{"action":"synchronize","pull_request":{"number":3,"head":{"ref":"x","sha":"${sha}"},"base":{"ref":"main"}},"repository":{"full_name":"o/r"}}`;
    const real = [prFile({ path: 'main.go', patch: '@@ -1 +1 @@\n+x' })];

    const stale = new CaptureLog();
    await engine(new FakeGH(real, { headSha: 'newsha' }), '[]', { log: stale }).kickoff(body('oldsha'));
    expect(stale.infos).toContain('stale review skipped (superseded by a newer push)');
    expect(stale.infos).not.toContain('review published');

    const current = new CaptureLog();
    await engine(new FakeGH(real, { headSha: 'samesha' }), '[]', { log: current }).kickoff(body('samesha'));
    expect(current.infos).toContain('review published');

    const errored = new CaptureLog();
    await engine(new FakeGH(real, { headShaError: new Error('boom') }), '[]', { log: errored }).kickoff(body('oldsha'));
    expect(errored.infos).toContain('review published'); // best-effort: lookup error proceeds
  });

  it('logs a deny decision without running the model', async () => {
    const gh = new FakeGH([prFile({ path: 'a.go', patch: 'x' }), prFile({ path: 'b.go', patch: 'x' })]);
    const log = new CaptureLog();
    const e = engine(gh, '[]', { maxFiles: 1, log });
    const body = '{"action":"opened","pull_request":{"number":9,"head":{"ref":"feature/x"}},"repository":{"full_name":"o/r"}}';
    await e.kickoff(body);
    expect(log.infos).toContain('review denied');
    // Deny publishes a "please split" summary + a neutral check, no inline review.
    expect(gh.reviews).toHaveLength(0);
    expect(gh.upserts[0]!.body).toContain('too large for automated review');
    expect(gh.checkRuns[0]!.conclusion).toBe('neutral');
  });
});

describe('review pipeline (canned findings)', () => {
  it('dedups every lens to one finding and scores it', async () => {
    const canned = '[{"file":"main.go","line":10,"dimension":"runtime_safety","severity":"major","message":"nil deref","confidence":0.9}]';
    const files = [prFile({ path: 'main.go', patch: '@@ -1 +1 @@\n+x', status: 'modified' })];
    const { card } = await runReview(engine(new FakeGH(), canned), files, null);
    expect(card.total).toBe(1);
    expect(card.overall).toBe(Level.Yellow);
  });

  it('drops a low-confidence finding and an empty lens', async () => {
    const files = [prFile({ path: 'main.go', patch: '+x' })];
    const low = '[{"file":"main.go","line":10,"dimension":"security","severity":"critical","message":"x","confidence":0.2}]';
    const dropped = await runReview(engine(new FakeGH(), low), files, null);
    expect(dropped.card.total).toBe(0);
    expect(dropped.card.overall).toBe(Level.Green);

    const empty = await runReview(engine(new FakeGH(), '[]'), files, null);
    expect(empty.card.total).toBe(0);
  });
});

describe('standards discovery', () => {
  const stdOverrides = { standardsEnabled: true, standardsGlobs: ['AGENTS.md'], standardsMaxBytes: 100_000 };

  it('discovers, distills, and caches the repo standards', async () => {
    const rules = '[{"id":"R1","dimension":"security","summary":"no secrets","source":"AGENTS.md"}]';
    const gh = new FakeGH([prFile({ path: 'src/a.ts', patch: '+x' })], {
      treeEntries: [{ path: 'AGENTS.md', sha: 's1', type: 'blob' }],
      fileContents: { 'AGENTS.md': '# rules\nNo secrets in code.' },
    });
    const e = engine(gh, rules, stdOverrides);
    const std = await discoverStandards(e, 'o', 'r', 'sha', [prFile({ path: 'src/a.ts' })]);
    expect(std).not.toBeNull();
    expect(std!.rules.map((r) => r.id)).toEqual(['R1']);
    expect(std!.sourceList()).toEqual(['AGENTS.md']);
    // Cached on the docs SHA: a second call reuses the memoized result (no re-distill).
    const again = await discoverStandards(e, 'o', 'r', 'sha', [prFile({ path: 'src/a.ts' })]);
    expect(again).toBe(std);
  });

  it('degrades to generic (null) on a truncated tree', async () => {
    const gh = new FakeGH([], {
      treeEntries: [{ path: 'AGENTS.md', sha: 's1', type: 'blob' }],
      treeTruncated: true,
    });
    expect(await discoverStandards(engine(gh, '[]', stdOverrides), 'o', 'r', 'sha', [])).toBeNull();
  });

  it('returns null when standards are disabled', async () => {
    const e = engine(new FakeGH(), '[]', { standardsEnabled: false });
    expect(await discoverStandards(e, 'o', 'r', 'sha', [])).toBeNull();
  });
});

describe('diff formatting', () => {
  it('renders a header per file and notes an omitted patch', () => {
    const out = formatDiff([
      prFile({ path: 'a.go', status: 'modified', patch: '@@ -1 +1 @@\n-old\n+new' }),
      prFile({ path: 'logo.png', status: 'added', patch: '' }),
    ]);
    expect(out).toContain('### a.go (modified)');
    expect(out).toContain('+new');
    expect(out).toContain('### logo.png (added)');
    expect(out).toContain('(no textual diff available)');
  });

  it('renders a rename header and sizes a fence past embedded backticks', () => {
    const out = formatDiff([prFile({ path: 'b.go', status: 'renamed', previousPath: 'a.go', patch: '+x' })]);
    expect(out).toContain('### b.go (renamed from a.go)');
    expect(maxBacktickRun('a ``` b `````')).toBe(5);
    expect(maxBacktickRun('no ticks')).toBe(0);
  });
});

describe('model tier selection', () => {
  it('returns the tier model when present', () => {
    const base = new FakeLlm('[]');
    const code = new FakeLlm('[]');
    const eng = new Engine({ baseLlm: base, codeLlm: code });
    expect(modelForTier(eng, Tier.Base)).toBe(base);
    expect(modelForTier(eng, Tier.Code)).toBe(code);
  });

  it('throws when the tier model is not configured', () => {
    const eng = new Engine({});
    expect(() => modelForTier(eng, Tier.Code)).toThrow(/review model not configured/);
  });
});
