/**
 * The reusable engine behind the PR-fixing agents (lint-fixer, coverage-fixer, …).
 *
 * It owns the event-driven loop — kickoff -> suspend -> CI resume -> loop or finish —
 * plus the apply mechanics and attempt counting. Each concrete agent supplies a
 * {@link Spec} (triage fn, analyze fn, branch/label/check names). State lives on GitHub;
 * there is no local store. The CI-wait suspend/resume itself is owned by the `Driver`
 * (ADK long-running + the in-memory registry).
 */
import type { BaseLlm } from '@google/adk';

import { parseCheckRunEvent } from '../../githubapi/client';
import type { Author } from '../../gitrepo/repo';
import type { Message, Notifier } from '../../notify/notify';
import {
  type ApplyConfig,
  type ApplyResult,
  type FileEdit,
  type GitHub,
  commit,
  openRepo,
} from './applyfix';
import { Driver, type RunParams } from './driver';
import { parseKickoff } from './envelope';

export type { FileEdit } from './applyfix';

/**
 * One file and the items to address in it (lint problems, uncovered regions, …) — the
 * normalized output of a Spec's triage step.
 */
export interface FileWork {
  path: string;
  items: string[];
}

/** Normalizes an arbitrary tool report into per-file work (LLM-backed). */
export type TriageFunc = (llm: BaseLlm, report: string) => Promise<FileWork[]>;

/**
 * What an AnalyzeFunc receives. `repoDir` is the checked-out working tree: analyze reads
 * source from it (and may explore it), and the engine commits whatever edits are
 * returned from the same checkout. `llm` is the default (planning) model; `codeLlm` is
 * the (often larger) model for writing code.
 */
export interface AnalyzeInput {
  llm: BaseLlm;
  codeLlm: BaseLlm | null;
  repoDir: string;
  work: FileWork[];
  feedback: string; // previous attempt's CI failure, on retry
  log?: Logger | null; // structured logger for skipped/unreadable files (optional)
}

/** Return the code-change model, falling back to the default when none is set. */
export function coder(input: AnalyzeInput): BaseLlm {
  return input.codeLlm ?? input.llm;
}

/** Produces the whole-file edits to apply (rewritten source, new tests, …). */
export type AnalyzeFunc = (input: AnalyzeInput) => Promise<FileEdit[]>;

/** Per-workflow configuration that turns the engine into a concrete fixing agent. */
export interface Spec {
  name: string; // "lint" | "coverage"
  branch: string; // e.g. automation-agent/lint-fix
  label: string; // e.g. automation-agent
  checkName: string; // e.g. agent-lint-verify
  commitMessage: string;
  prTitle: string;
  successTitle: string; // notification title on success
  reviewTitle: string; // notification title when human review is needed
  triage: TriageFunc;
  analyze: AnalyzeFunc;
}

/** Structured logger the engine/driver emit through (optional). */
export interface Logger {
  info(msg: string, fields?: Record<string, unknown>): void;
  warn(msg: string, fields?: Record<string, unknown>): void;
}

function defaultCloneUrl(owner: string, repo: string): string {
  return `https://github.com/${owner}/${repo}.git`;
}

const DEFAULT_AUTHOR: Author = {
  name: 'automation-agent',
  email: 'automation-agent@users.noreply.github.com',
};

const DEFAULT_CI_TIMEOUT_MS = 90 * 60 * 1000;

/**
 * Runtime dependencies shared by all engines. `codeLlm` is the model for the code-change
 * steps (typically larger); it falls back to `llm` when null. `ciTimeoutMs` bounds how
 * long a suspended run waits for its CI result.
 */
export interface Deps {
  llm: BaseLlm;
  codeLlm?: BaseLlm | null;
  gh: GitHub;
  notify?: Notifier | null;
  token?: string;
  maxIter?: number;
  ciTimeoutMs?: number;
  author?: Author;
  log?: Logger | null;
  cloneUrl?: (owner: string, repo: string) => string;
}

/** The resolved deps after defaults are applied. */
export interface ResolvedDeps {
  llm: BaseLlm;
  codeLlm: BaseLlm | null;
  gh: GitHub;
  notify: Notifier | null;
  token: string;
  maxIter: number;
  ciTimeoutMs: number;
  author: Author;
  log: Logger | null;
  cloneUrl: (owner: string, repo: string) => string;
}

/**
 * The normalized resume context derived from a check_run webhook. The parked run already
 * holds owner/repo/branch from kickoff, so resume only needs the PR identity, the
 * conclusion, and the CI output (used as retry feedback).
 */
export interface ResumeInput {
  fullRepo: string;
  prNumber: number;
  conclusion: string;
  outputText: string;
}

/** Runs one Spec's event-driven fix loop. */
export class Engine {
  readonly spec: Spec;
  readonly d: ResolvedDeps;
  readonly driver: Driver;

  constructor(spec: Spec, d: ResolvedDeps) {
    this.spec = spec;
    this.d = d;
    this.driver = new Driver(this);
  }

  /** The PR label this engine's workflow uses. */
  label(): string {
    return this.spec.label;
  }

  /** The agent verify check this engine resumes on. */
  checkName(): string {
    return this.spec.checkName;
  }

  /** Handle a kickoff envelope: start a suspended fix run (apply -> await CI). */
  async kickoff(raw: Buffer | string): Promise<void> {
    const k = parseKickoff(raw);
    this.d.log?.info('fix kickoff', { workflow: this.spec.name, repo: k.repo });
    await this.driver.kickoff(k);
  }

  /**
   * Handle a GitHub check_run webhook. No-op unless the event is this engine's check
   * completing — so multiple engines can each be handed the event.
   */
  async resume(raw: Buffer | string): Promise<void> {
    const ev = parseCheckRunEvent(raw);
    if (ev.checkName !== this.spec.checkName || ev.status !== 'completed') {
      return;
    }
    await this.driver.resume({
      fullRepo: ev.repoFullName,
      prNumber: ev.prNumber,
      conclusion: ev.conclusion,
      outputText: ev.outputText,
    });
  }

  /**
   * Run a single fix attempt: triage -> checkout -> analyze -> commit, returning the
   * resulting PR. The body the apply_fix tool invokes.
   */
  async attemptOnce(rp: RunParams): Promise<ApplyResult> {
    const work = await this.spec.triage(this.d.llm, rp.report);

    const cfg: ApplyConfig = {
      owner: rp.owner,
      repo: rp.repo,
      cloneUrl: this.d.cloneUrl(rp.owner, rp.repo),
      token: this.d.token,
      base: rp.base,
      branch: this.spec.branch,
      newBranch: rp.newBranch,
      label: this.spec.label,
      commitMessage: this.spec.commitMessage,
      prTitle: this.spec.prTitle,
      prBody: prBody(this.spec, work),
      author: this.d.author,
    };

    const repo = await openRepo(cfg);
    try {
      const edits = await this.spec.analyze({
        llm: this.d.llm,
        codeLlm: this.d.codeLlm,
        repoDir: repo.dir(),
        work,
        feedback: rp.feedback,
        log: this.d.log,
      });
      return await commit(this.d.gh, repo, cfg, edits);
    } finally {
      const { rmSync } = await import('node:fs');
      rmSync(repo.dir(), { recursive: true, force: true });
    }
  }

  /** Best-effort notification; no-op when no notifier is configured. */
  async notify(title: string, text: string, link: string): Promise<void> {
    if (!this.d.notify) {
      return;
    }
    const m: Message = { title, text, link };
    await this.d.notify.notify(m);
  }
}

/** Build an engine, applying defaults. */
export function newEngine(spec: Spec, d: Deps): Engine {
  const resolved: ResolvedDeps = {
    llm: d.llm,
    codeLlm: d.codeLlm ?? d.llm,
    gh: d.gh,
    notify: d.notify ?? null,
    token: d.token ?? '',
    maxIter: d.maxIter && d.maxIter > 0 ? d.maxIter : 3,
    ciTimeoutMs: d.ciTimeoutMs && d.ciTimeoutMs > 0 ? d.ciTimeoutMs : DEFAULT_CI_TIMEOUT_MS,
    author: d.author && d.author.name !== '' ? d.author : DEFAULT_AUTHOR,
    log: d.log ?? null,
    cloneUrl: d.cloneUrl ?? defaultCloneUrl,
  };
  return new Engine(spec, resolved);
}

export function pullUrl(fullRepo: string, num: number): string {
  return `https://github.com/${fullRepo}/pull/${num}`;
}

function prBody(spec: Spec, work: FileWork[]): string {
  const lines = [`Automated ${spec.name} fix by automation-agent.\n`, 'Files addressed:'];
  for (const f of work) {
    lines.push(`- \`${f.path}\` (${f.items.length} item(s))`);
  }
  return lines.join('\n') + '\n';
}
