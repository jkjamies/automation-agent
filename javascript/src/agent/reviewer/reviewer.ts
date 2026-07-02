/**
 * The reviewer engine: dependencies, the intake pipeline (skip / deny / review), and the kickoff
 * entry point.
 *
 * Unlike the lint/coverage fixers, the reviewer is not a suspend/resume fix loop: it is mostly
 * one-shot per `pull_request` event and does not park on await_ci. Its long LLM compute runs
 * in-request via the execution transport (Kind.Review → /internal/dispatch), so CPU stays allocated
 * on Cloud Run.
 *
 * The flow per pull_request event: parse it, apply the trigger and skip rules, fetch the changed
 * files via the REST API, filter generated/vendored churn, and apply the two-dimensional size gate
 * to reach a decision (skip / deny / review). A review fans out the category lenses + glue pass and
 * scores the findings (count-based scorecard). Publishing the scored review to the PR is a
 * follow-up.
 */

import type { BaseLlm } from '@google/adk';

import { type PRFile, type PullRequestEvent, parsePullRequestEvent } from '../../githubapi/client';
import { FileFilter } from './filter';
import { clampThreshold } from './findings';
import { runReview } from './review';
import { levelGlyph } from './scorecard';
import { oversize } from './sizegate';

/**
 * Marks branches the fixers create (they push to automation-agent/...). The reviewer skips PRs from
 * these branches so it never reviews the fixers' own PRs in a loop. Mirrors the AGENT_PR_LABEL
 * namespace.
 */
export const OWN_BRANCH_PREFIX = 'automation-agent/';

/** Structured logger the engine emits through. */
export interface Logger {
  debug?(msg: string, fields?: Record<string, unknown>): void;
  info(msg: string, fields?: Record<string, unknown>): void;
  warn(msg: string, fields?: Record<string, unknown>): void;
}

/** A logger that drops everything — the fallback when none is injected. */
const NOOP_LOGGER: Logger = { debug() {}, info() {}, warn() {} };

/**
 * The slice of `githubapi.Client` the reviewer needs to detect and analyze a PR: read the changed
 * files (with patches) and read the head SHA (to detect a task superseded by a newer push). A local
 * interface keeps the engine testable with a fake.
 */
export interface GitHubClient {
  listPRFiles(owner: string, repo: string, num: number): Promise<PRFile[]>;
  pullRequestHeadSha(owner: string, repo: string, num: number): Promise<string>;
}

/** Wires the reviewer engine. */
export interface Deps {
  /**
   * The REVIEW_ENABLED kill switch. When false the engine accepts and acknowledges pull_request
   * events but does no review work — the default and the rollback posture.
   */
  enabled?: boolean;
  gh?: GitHubClient | null;
  baseLlm?: BaseLlm | null;
  codeLlm?: BaseLlm | null;
  /**
   * Drops findings below this confidence before scoring (the phase-1 verify gate). A non-positive
   * value keeps everything.
   */
  minConfidence?: number;
  /** Skips draft PRs unless the triggering action is ready_for_review. */
  skipDrafts?: boolean;
  /** Drops generated/vendored/lockfile/minified/binary paths before sizing. */
  excludeGlobs?: string[];
  /**
   * The two-dimensional size-gate caps; a non-positive value disables that dimension.
   */
  maxFiles?: number;
  maxDiffBytes?: number;
  /**
   * Toggles standards-aware review: discover the reviewed repo's own convention docs, distill them,
   * and steer the lenses off them.
   */
  standardsEnabled?: boolean;
  standardsGlobs?: string[];
  standardsMaxBytes?: number;
  /**
   * When true (REVIEW_UNCITED_MODE=drop), drops a conformance finding that cites no real repo rule;
   * otherwise (default) it is demoted to nitpick.
   */
  uncitedDrop?: boolean;
  log?: Logger | null;
}

/** The outcome of intake for one pull_request event. */
export const DecisionKind = {
  Skip: 0, // not reviewable (trigger/skip rule or empty diff)
  Deny: 1, // reviewable but too large — deny, don't degrade
  Review: 2, // proceed to review the kept files
} as const;
export type DecisionKind = (typeof DecisionKind)[keyof typeof DecisionKind];

/**
 * The result of the intake pipeline. files/diffBytes are the filtered review surface (set for deny
 * and review); reason explains a skip or a deny.
 */
export interface Decision {
  kind: DecisionKind;
  reason: string;
  files: PRFile[];
  diffBytes: number;
}

/** Runs the PR code-review workflow for one pull_request event. */
export class Engine {
  readonly enabled: boolean;
  // gh / baseLlm / codeLlm are required for real work; kickoff guards each with a controlled error
  // before any use (disabled/skip/deny paths never touch the missing one), so the collaborators can
  // treat them as always-present.
  private readonly gh: GitHubClient | null;
  readonly baseLlm: BaseLlm | null;
  readonly codeLlm: BaseLlm | null;
  readonly minConfidence: number;
  private readonly skipDrafts: boolean;
  private readonly filter: FileFilter;
  private readonly maxFiles: number;
  private readonly maxDiffBytes: number;
  readonly standardsEnabled: boolean;
  readonly standardsGlobs: string[];
  readonly standardsMaxBytes: number;
  readonly uncitedDrop: boolean;
  readonly log: Logger;

  constructor(d: Deps) {
    this.enabled = d.enabled ?? false;
    this.gh = d.gh ?? null;
    this.baseLlm = d.baseLlm ?? null;
    this.codeLlm = d.codeLlm ?? null;
    this.minConfidence = clampThreshold(d.minConfidence ?? 0);
    this.skipDrafts = d.skipDrafts ?? true;
    this.filter = new FileFilter(d.excludeGlobs ?? []);
    this.maxFiles = d.maxFiles ?? 0;
    this.maxDiffBytes = d.maxDiffBytes ?? 0;
    this.standardsEnabled = d.standardsEnabled ?? false;
    this.standardsGlobs = d.standardsGlobs ?? [];
    this.standardsMaxBytes = d.standardsMaxBytes ?? 0;
    this.uncitedDrop = d.uncitedDrop ?? false;
    this.log = d.log ?? NOOP_LOGGER;
  }

  /**
   * Handle one pull_request webhook delivery (Kind.Review). The root dispatcher calls it with the
   * raw event payload; it runs in-request via the execution transport.
   *
   * When disabled (REVIEW_ENABLED=false, the default) it no-ops, so the feature is dark by default
   * and REVIEW_ENABLED is the kill switch. When enabled it runs intake and either skips, denies
   * (too large), or scores a review.
   */
  async kickoff(raw: Buffer | string): Promise<void> {
    if (!this.enabled) {
      this.log.debug?.('reviewer disabled (REVIEW_ENABLED=false); ignoring pull_request event', {
        bytes: rawLength(raw),
      });
      return;
    }
    // An enabled engine needs a client to fetch the diff (both deny and review paths reach it);
    // without it, raise a controlled error rather than dereferencing a nil dependency.
    if (this.gh === null) {
      throw new Error('reviewer: enabled but GitHub client not configured');
    }
    let ev: PullRequestEvent;
    try {
      ev = parsePullRequestEvent(raw);
    } catch (err) {
      throw new Error(`reviewer: ${errMsg(err)}`);
    }
    const d = await this.decide(ev);
    const pr = `${ev.repoFullName}#${ev.number}`;
    // decide() already validated the full name before reaching a deny/review decision, so a
    // malformed name here means skip.
    const { owner, repo } = splitFullName(ev.repoFullName);
    // Coalesce-to-latest: a deny/review acts on the event's SHA, so if a newer push has superseded
    // it, skip rather than produce a stale review. A skip produced nothing.
    if (d.kind !== DecisionKind.Skip && (await this.superseded(owner, repo, ev))) {
      this.log.info('stale review skipped (superseded by a newer push)', { pr, eventSha: ev.headSha });
      return;
    }
    if (d.kind === DecisionKind.Skip) {
      this.log.info('review skipped', { pr, action: ev.action, reason: d.reason });
    } else if (d.kind === DecisionKind.Deny) {
      // Too large to review: it is denied, not degraded. Publishing the "please split" notice is a
      // follow-up.
      this.log.info('review denied', {
        pr,
        files: d.files.length,
        diffBytes: d.diffBytes,
        reason: d.reason,
      });
    } else {
      // Review needs both tier models; the deny branch above does not.
      if (this.baseLlm === null || this.codeLlm === null) {
        throw new Error('reviewer: enabled but review models not configured');
      }
      const { card } = await runReview(this, d.files);
      // Publishing the scored review to the PR is a follow-up.
      this.log.info('review scored', {
        pr,
        files: d.files.length,
        overall: levelGlyph(card.overall),
        findings: card.total,
      });
    }
  }

  /**
   * Run the deterministic intake pipeline for one event: trigger gate → skip rules → fetch files →
   * filter → size gate. It performs no model calls and posts nothing.
   */
  async decide(ev: PullRequestEvent): Promise<Decision> {
    if (!['opened', 'reopened', 'synchronize', 'ready_for_review'].includes(ev.action)) {
      return skip(`action "${ev.action}" is not a reviewed trigger`);
    }
    if (this.skipDrafts && ev.draft && ev.action !== 'ready_for_review') {
      return skip('draft PR (REVIEW_SKIP_DRAFTS)');
    }
    if (ev.headRef.startsWith(OWN_BRANCH_PREFIX)) {
      return skip(`agent's own PR (head "${ev.headRef}")`);
    }
    if (ev.labels.includes('skip-review')) {
      return skip('skip-review label');
    }
    if (isDependencyBot(ev.authorLogin)) {
      return skip(`dependency-bot PR (${ev.authorLogin})`);
    }

    const { owner, repo, ok } = splitFullName(ev.repoFullName);
    if (!ok) {
      throw new Error(`reviewer: malformed repository full name "${ev.repoFullName}"`);
    }
    // kickoff guarantees a client before decide runs.
    const gh = this.gh!;
    let files: PRFile[];
    try {
      files = await gh.listPRFiles(owner, repo, ev.number);
    } catch (err) {
      throw new Error(`reviewer: list PR files: ${errMsg(err)}`);
    }
    const { kept, diffBytes } = this.filter.apply(files);
    if (kept.length === 0) {
      return skip(`no reviewable files after exclude filter (${files.length} changed)`);
    }
    const { reason, denied } = oversize(kept.length, diffBytes, this.maxFiles, this.maxDiffBytes);
    if (denied) {
      return { kind: DecisionKind.Deny, reason, files: kept, diffBytes };
    }
    return { kind: DecisionKind.Review, reason: '', files: kept, diffBytes };
  }

  /**
   * Report whether a newer push has replaced the SHA this task was enqueued for. It is best-effort:
   * a missing event SHA or a lookup error yields false (proceed) so a transient failure never
   * suppresses a real review.
   */
  private async superseded(owner: string, repo: string, ev: PullRequestEvent): Promise<boolean> {
    if (ev.headSha === '') {
      return false;
    }
    const gh = this.gh!;
    let current: string;
    try {
      current = await gh.pullRequestHeadSha(owner, repo, ev.number);
    } catch (err) {
      this.log.warn('could not fetch current head SHA; proceeding with review', {
        pr: ev.repoFullName,
        err: errMsg(err),
      });
      return false;
    }
    return current !== '' && current !== ev.headSha;
  }
}

/** Build the reviewer engine from its dependencies. */
export function newEngine(d: Deps): Engine {
  return new Engine(d);
}

/** Build a skip decision with a formatted reason. */
function skip(reason: string): Decision {
  return { kind: DecisionKind.Skip, reason, files: [], diffBytes: 0 };
}

/**
 * Report whether the author is a known dependency-update bot. GitHub Apps post as "<name>[bot]".
 */
export function isDependencyBot(login: string): boolean {
  return login === 'dependabot[bot]' || login === 'renovate[bot]';
}

/**
 * Split an "owner/name" repository full name. Reports ok=false for anything that is not exactly one
 * owner and one non-empty name.
 */
export function splitFullName(full: string): { owner: string; repo: string; ok: boolean } {
  const idx = full.indexOf('/');
  if (idx < 0) {
    return { owner: '', repo: '', ok: false };
  }
  const owner = full.slice(0, idx);
  const repo = full.slice(idx + 1);
  if (owner === '' || repo === '' || repo.includes('/')) {
    return { owner: '', repo: '', ok: false };
  }
  return { owner, repo, ok: true };
}

/** The byte length of a raw payload (Buffer or string). */
function rawLength(raw: Buffer | string): number {
  return typeof raw === 'string' ? Buffer.byteLength(raw, 'utf-8') : raw.length;
}

/** Extract a message from a thrown value. */
function errMsg(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}
