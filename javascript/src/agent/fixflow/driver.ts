/**
 * The CI-wait suspend/resume loop on ADK long-running tools.
 *
 * The Driver owns the long-run agent and all policy — retry vs give up, attempt
 * counting, the per-run timeout — while the suspended-run state lives in an injected
 * {@link ParkStore} (memory by default, sqlite/firestore for durability). The agent's
 * Sequencer model only emits a fixed apply_fix -> await_ci sequence.
 *
 * Lifecycle: kickoff applies a fix and parks on await_ci (a {@link ParkRecord} indexed by
 * PR key). A check_run webhook drives resume, which atomically claims the parked run and
 * either notifies success, resumes for another attempt, or gives up at maxIter. If CI
 * never reports, a soft per-run timer fires onTimeout; a durable catch-all, sweepTimeouts
 * (driven by the periodic /internal/sweep), resolves runs whose timer was lost to a
 * restart. The store's single-winner claim guarantees exactly one of those paths resolves
 * a given run.
 *
 * The await_ci long-running tool returns `null` to park; on resume the real CI outcome is
 * fed back as its function response. The apply_fix tool returns `{ error }` on failure so
 * the Sequencer's apply-error branch can conclude. Run params are loaded from the store by
 * session id, never from model-controlled args, so a misbehaving model cannot redirect
 * which repo or branch is edited.
 */
import { randomUUID } from 'node:crypto';

import { FunctionTool, LlmAgent, LongRunningFunctionTool } from '@google/adk';

import { type ParkRecord, type ParkStore, MemoryParkStore } from '../setup/parkstore';
import { Type } from '../setup/genai';
import { LongRunDriver, Sequencer, type DriveResult } from '../setup/longrun';
import type { Comparison } from '../../githubapi/client';
import type { Engine, Logger, ResumeInput } from './engine';
import type { Kickoff } from './envelope';
import { type SummaryInput, TerminalOutcome, buildSummaryText } from './summary';

const TOOL_APPLY_FIX = 'apply_fix';
const TOOL_AWAIT_CI = 'await_ci';

/**
 * Per-run inputs the apply_fix tool needs. Owned by the Driver (serialized into the park
 * record, keyed by session id) and never model-controlled, so a misbehaving model cannot
 * redirect which repo or branch is edited.
 */
export interface RunParams {
  owner: string;
  repo: string;
  fullRepo: string;
  base: string;
  report: string;
  feedback: string; // previous attempt's CI failure, on retry
  newBranch: boolean; // true on kickoff (create from base); false on retry
}

function runParamsToJson(rp: RunParams): string {
  return JSON.stringify(rp);
}

function runParamsFromJson(s: string): RunParams {
  return JSON.parse(s) as RunParams;
}

/** Runs a Spec's CI-wait loop on ADK long-running suspend/resume, backed by a park store. */
export class Driver {
  /** The durable (or in-memory) parked-run store. Exposed for tests to inspect/seed. */
  readonly store: ParkStore;
  private readonly timeoutMs: number;
  private readonly lr: LongRunDriver;
  // Soft per-run timers keyed by PR key. A restart loses these; sweepTimeouts is the
  // durable catch-all that frees runs whose timer was dropped.
  private readonly timers = new Map<string, ReturnType<typeof setTimeout>>();

  constructor(private readonly engine: Engine) {
    this.timeoutMs = engine.d.ciTimeoutMs;
    this.store = engine.d.parkStore ?? new MemoryParkStore();

    const seqModel = new Sequencer(
      TOOL_APPLY_FIX,
      TOOL_AWAIT_CI,
      // The Driver only resumes a run when it has already decided to retry, so a
      // resumed failure always means "apply again". (success/timeout never resume.)
      (resp) => String(resp.conclusion) === 'failure',
    );

    const applyFixTool = new FunctionTool({
      name: TOOL_APPLY_FIX,
      description: 'Apply the fix for the current run and open/update its PR.',
      parameters: { type: Type.OBJECT, properties: {} },
      execute: (_input: unknown, ctx?: { sessionId?: string }) => this.applyFixTool(ctx),
    });
    const awaitCiTool = new LongRunningFunctionTool({
      name: TOOL_AWAIT_CI,
      description: 'Wait for the PR CI result. Returns when CI reports.',
      parameters: {
        type: Type.OBJECT,
        properties: {
          pr_number: { type: Type.INTEGER },
          head_sha: { type: Type.STRING },
        },
      },
      // Returning null parks the run; the real CI result arrives via resume.
      execute: () => null,
    });

    const fixer = new LlmAgent({
      name: `fixer_${engine.spec.name}`,
      model: seqModel,
      instruction: 'Apply the fix, then wait for CI. If CI fails, apply again.',
      tools: [applyFixTool, awaitCiTool],
    });
    this.lr = new LongRunDriver(
      `fixflow-${engine.spec.name}`,
      'fixer',
      fixer,
      engine.d.sessionService ?? undefined,
    );
  }

  // --- tools -------------------------------------------------------------

  /**
   * Run one fix attempt for the calling session. The run params are loaded from the store
   * by session id (Driver-owned), so the model's args cannot influence the target. Returns
   * `{ error }` on failure so the Sequencer's apply-error branch can conclude.
   */
  private async applyFixTool(ctx?: { sessionId?: string }): Promise<Record<string, unknown>> {
    try {
      const sid = ctx?.sessionId ?? '';
      const rec = await this.store.get(sid);
      if (!rec) {
        throw new Error(`apply_fix: no run params for session ${JSON.stringify(sid)}`);
      }
      const res = await this.engine.attemptOnce(runParamsFromJson(rec.params));
      return { pr_number: res.pr.number, head_sha: res.headSha };
    } catch (err) {
      return { error: (err as Error).message };
    }
  }

  // --- lifecycle ---------------------------------------------------------

  /** Start a new suspended run: apply the fix, then park awaiting CI. */
  async kickoff(k: Kickoff): Promise<void> {
    const sid = this.newSessionId();
    const rp: RunParams = {
      owner: k.owner(),
      repo: k.name(),
      fullRepo: k.repo,
      base: k.base,
      report: k.reportText(),
      feedback: '',
      newBranch: true,
    };
    await this.putParams(sid, rp);
    let res: DriveResult;
    try {
      res = await this.lr.start(sid, 'Apply the fix and wait for CI.');
    } catch (err) {
      await this.clear(sid);
      throw err;
    }
    await this.afterDrive(sid, k.repo, res, 1);
  }

  /** React to a CI conclusion for a parked run. */
  async resume(input: ResumeInput): Promise<void> {
    if (input.prNumber === 0) {
      throw new Error('resume: missing PR number');
    }
    // Only success/failure are actionable. Leave the run parked otherwise.
    if (input.conclusion !== 'success' && input.conclusion !== 'failure') {
      this.log('info', 'ignoring non-actionable conclusion', {
        repo: input.fullRepo,
        conclusion: input.conclusion,
      });
      return;
    }

    const key = prKey(input.fullRepo, input.prNumber);
    const run = await this.store.resolveByPrKey(key);
    if (!run) {
      // Late, duplicate, raced with the timeout/sweep, or after a restart — nothing to do.
      this.log('info', 'resume: no parked run', { pr: key, conclusion: input.conclusion });
      return;
    }
    this.stopTimer(key); // the webhook won; cancel the soft timer for this run

    if (input.conclusion === 'success') {
      await this.clear(run.sessionId);
      this.log('info', 'fix succeeded', { repo: input.fullRepo, pr: input.prNumber });
      await this.terminalNotify(
        TerminalOutcome.Success,
        this.engine.spec.successTitle,
        run,
        input.fullRepo,
        input.prNumber,
        '',
      );
      return;
    }

    // failure
    if (run.attempts >= this.engine.d.maxIter) {
      await this.clear(run.sessionId);
      this.log('warn', 'fix exhausted attempts', {
        repo: input.fullRepo,
        pr: input.prNumber,
        attempts: run.attempts,
      });
      await this.terminalNotify(
        TerminalOutcome.Exhausted,
        this.engine.spec.reviewTitle,
        run,
        input.fullRepo,
        input.prNumber,
        input.outputText,
      );
      return;
    }

    await this.updateForRetry(run.sessionId, input.outputText);
    let res: DriveResult;
    try {
      res = await this.lr.resume(run.sessionId, run.callId, TOOL_AWAIT_CI, {
        conclusion: input.conclusion,
        output: input.outputText,
      });
    } catch (err) {
      await this.clear(run.sessionId);
      throw err;
    }
    this.log('info', 'fix retrying', {
      repo: input.fullRepo,
      pr: input.prNumber,
      attempt: run.attempts + 1,
    });
    await this.afterDrive(run.sessionId, input.fullRepo, res, run.attempts + 1);
  }

  /**
   * Fires (from the soft timer) when a parked run's CI never reports. Claims the run,
   * frees it, and asks for human review. Invoked detached (`void onTimeout`) by the timer,
   * so a notifier rejection is swallowed here rather than surfacing as a process-level
   * unhandledRejection.
   */
  async onTimeout(key: string): Promise<void> {
    const run = await this.store.resolveByPrKey(key);
    this.timers.delete(key); // the timer has fired; drop its handle
    if (!run) {
      return; // already resolved by a webhook or the sweep
    }
    await this.clear(run.sessionId);
    const [fullRepo, pr] = splitPrKey(key);
    this.log('warn', 'fix timed out waiting for CI', {
      repo: fullRepo,
      pr,
      timeoutMs: this.timeoutMs,
    });
    try {
      await this.terminalNotify(
        TerminalOutcome.Timeout,
        this.engine.spec.reviewTitle,
        run,
        fullRepo,
        pr,
        '',
      );
    } catch (err) {
      this.log('warn', 'timeout notification failed', { repo: fullRepo, pr, err: String(err) });
    }
  }

  /**
   * Durable timeout catch-all: free every parked run whose CI never reported, driven by
   * the periodic /internal/sweep. Covers runs whose soft timer was lost to a restart. Each
   * run is claimed exactly once by the store, so this never races the webhook or the timer.
   */
  async sweepTimeouts(): Promise<void> {
    const cutoff = new Date(Date.now() - this.timeoutMs);
    const stale = await this.store.sweep(cutoff);
    for (const run of stale) {
      this.stopTimer(run.prKey);
      await this.clear(run.sessionId);
      const [fullRepo, pr] = splitPrKey(run.prKey);
      this.log('warn', 'fix swept; CI never reported', { repo: fullRepo, pr });
      try {
        await this.terminalNotify(
          TerminalOutcome.Timeout,
          this.engine.spec.reviewTitle,
          run,
          fullRepo,
          pr,
          '',
        );
      } catch (err) {
        this.log('warn', 'sweep notification failed', { repo: fullRepo, pr, err: String(err) });
      }
    }
  }

  /** Number of currently parked runs (test/inspection utility). */
  parkedCount(): Promise<number> {
    return this.store.parkedCount();
  }

  /**
   * Build and send the status-aware summary for a finished run: the outcome framing, the
   * original targeted findings, and what actually changed on the PR.
   */
  private async terminalNotify(
    outcome: TerminalOutcome,
    title: string,
    run: ParkRecord,
    fullRepo: string,
    prNumber: number,
    lastOutput: string,
  ): Promise<void> {
    const input: SummaryInput = {
      outcome,
      workflow: this.engine.spec.name,
      fullRepo,
      prNumber,
      attempts: run.attempts,
      lastOutput,
      timeout: formatTimeout(this.timeoutMs),
      checkName: this.engine.spec.checkName,
      report: '',
      changed: { totalCommits: 0, files: [] },
    };
    try {
      const rp = runParamsFromJson(run.params);
      input.report = rp.report;
      input.changed = await this.gatherChanges(rp);
    } catch (err) {
      this.log('warn', 'decode run params for summary failed; sending without findings/diff', {
        session: run.sessionId,
        err: String(err),
      });
    }
    await this.engine.notify(title, buildSummaryText(input), pullUrl(fullRepo, prNumber));
  }

  /**
   * Best-effort fetch of the PR branch's base...head diff for a terminal summary. On error
   * returns an empty comparison so the summary still reports the attempt count and findings.
   */
  private async gatherChanges(rp: RunParams): Promise<Comparison> {
    try {
      return await this.engine.d.gh.compare(rp.owner, rp.repo, rp.base, this.engine.spec.branch);
    } catch (err) {
      this.log('warn', 'compare for summary failed', { repo: rp.fullRepo, err: String(err) });
      return { totalCommits: 0, files: [] };
    }
  }

  // --- internals ---------------------------------------------------------

  /**
   * Inspect a drive's outcome and either surface an apply error or park the run (and its
   * timeout) under its PR key.
   */
  private async afterDrive(
    sid: string,
    fullRepo: string,
    res: DriveResult,
    attempt: number,
  ): Promise<void> {
    const apply = res.toolResponses[TOOL_APPLY_FIX];
    if (apply && 'error' in apply) {
      await this.failApply(sid, fullRepo, String(apply.error));
      return;
    }
    if (res.parkedCallId === '') {
      await this.failApply(sid, fullRepo, 'run did not park on CI wait');
      return;
    }
    const pr = prNumberFrom(apply);
    if (pr === 0) {
      await this.failApply(sid, fullRepo, 'parked without a PR number');
      return;
    }
    await this.park(sid, prKey(fullRepo, pr), res.parkedCallId, attempt);
    this.log('info', 'fix applied; awaiting CI', { repo: fullRepo, pr, attempt });
  }

  /** Store a fresh run's params (not yet parked: empty PR key, zero attempts). */
  private async putParams(sid: string, rp: RunParams): Promise<void> {
    await this.store.put({
      sessionId: sid,
      prKey: '',
      callId: '',
      attempts: 0,
      params: runParamsToJson(rp),
      parkedAt: null,
    });
  }

  /** Record a run as parked under its PR key and arm its soft timeout. */
  private async park(sid: string, key: string, callId: string, attempt: number): Promise<void> {
    const rec = await this.store.get(sid);
    if (!rec) {
      // The run vanished mid-flight (e.g. a concurrent clear) — nothing to park.
      this.log('warn', 'park: no run params; skipping', { pr: key });
      return;
    }
    rec.prKey = key;
    rec.callId = callId;
    rec.attempts = attempt;
    rec.parkedAt = new Date();
    await this.store.put(rec);
    this.armTimer(key);
  }

  /**
   * Free a run that errored before it could park on CI (a push/PR/analyze failure, not a
   * CI failure), notify a human, then throw. Without the notification an apply error would
   * only reach the dispatcher's logger and never the review channel — a fix that can't even
   * open its PR would vanish silently.
   */
  private async failApply(sid: string, fullRepo: string, reason: string): Promise<never> {
    await this.clear(sid);
    try {
      await this.engine.notify(
        this.engine.spec.reviewTitle,
        `${fullRepo}: the ${this.engine.spec.name} fix could not be applied (${reason}). Please review.`,
        '',
      );
    } catch (err) {
      this.log('warn', 'apply-failure notification failed', { repo: fullRepo, err: String(err) });
    }
    throw new Error(`${fullRepo} ${this.engine.spec.name}: ${reason}`);
  }

  private log(level: keyof Logger, msg: string, fields: Record<string, unknown>): void {
    const l = this.engine.d.log;
    if (l) {
      l[level](msg, { workflow: this.engine.spec.name, ...fields });
    }
  }

  private newSessionId(): string {
    return randomUUID();
  }

  private async updateForRetry(sid: string, feedback: string): Promise<void> {
    const rec = await this.store.get(sid);
    if (!rec) {
      return;
    }
    const rp = runParamsFromJson(rec.params);
    rp.feedback = 'The previous attempt failed CI with:\n' + feedback;
    rp.newBranch = false;
    rec.params = runParamsToJson(rp);
    await this.store.put(rec);
  }

  /**
   * Terminal cleanup: drop the park record and the ADK session. Best-effort — errors are
   * logged but never unwind the caller, so a failed cleanup cannot strand a resolution.
   */
  private async clear(sid: string): Promise<void> {
    try {
      await this.store.delete(sid);
    } catch (err) {
      this.log('warn', 'clear: park-record delete failed', { session: sid, err: String(err) });
    }
    try {
      await this.lr.deleteSession(sid);
    } catch (err) {
      this.log('warn', 'clear: session delete failed', { session: sid, err: String(err) });
    }
  }

  private armTimer(key: string): void {
    const old = this.timers.get(key);
    if (old) {
      clearTimeout(old);
    }
    const t = setTimeout(() => {
      void this.onTimeout(key);
    }, this.timeoutMs);
    // Don't let the pending timer keep the process alive.
    t.unref?.();
    this.timers.set(key, t);
  }

  private stopTimer(key: string): void {
    const t = this.timers.get(key);
    if (t) {
      clearTimeout(t);
      this.timers.delete(key);
    }
  }
}

function prKey(fullRepo: string, num: number): string {
  return `${fullRepo}#${num}`;
}

function splitPrKey(key: string): [string, number] {
  const i = key.indexOf('#');
  if (i < 0) {
    return [key, 0];
  }
  const n = Number.parseInt(key.slice(i + 1), 10);
  return [key.slice(0, i), Number.isNaN(n) ? 0 : n];
}

function pullUrl(fullRepo: string, num: number): string {
  return `https://github.com/${fullRepo}/pull/${num}`;
}

/** Format a millisecond duration as a compact human string (e.g. `90m`, `1h`, `30s`). */
function formatTimeout(ms: number): string {
  if (ms > 0 && ms % 3_600_000 === 0) {
    return `${ms / 3_600_000}h`;
  }
  if (ms > 0 && ms % 60_000 === 0) {
    return `${ms / 60_000}m`;
  }
  if (ms > 0 && ms % 1_000 === 0) {
    return `${ms / 1_000}s`;
  }
  return `${ms}ms`;
}

function prNumberFrom(resp: Record<string, unknown> | undefined): number {
  if (!resp) {
    return 0;
  }
  const v = resp.pr_number;
  if (typeof v === 'boolean') {
    return 0;
  }
  if (typeof v === 'number') {
    return Math.trunc(v);
  }
  return 0;
}
