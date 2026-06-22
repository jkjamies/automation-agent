/**
 * The CI-wait suspend/resume loop on ADK long-running tools.
 *
 * The Driver owns the long-run agent, the in-memory parked-run registry, and each
 * session's run params. All policy — retry vs give up, attempt counting, the per-run
 * timeout — lives here; the agent's Sequencer model only emits a fixed
 * apply_fix -> await_ci sequence.
 *
 * Lifecycle: kickoff applies a fix and parks on await_ci (registered in the registry). A
 * check_run webhook drives resume, which atomically claims the parked run and either
 * notifies success, resumes for another attempt, or gives up at maxIter. If CI never
 * reports, the registry's per-run timer fires onTimeout, which frees the run and asks for
 * human review. There is no durable store: a process restart strands parked runs.
 *
 * The await_ci long-running tool returns `null` to park; on resume the real CI outcome
 * is fed back as its function response. The apply_fix tool returns `{ error }` on
 * failure so the Sequencer's apply-error branch can conclude.
 */
import { FunctionTool, LlmAgent, LongRunningFunctionTool } from '@google/adk';

import { Type } from '../setup/genai';
import { LongRunDriver, Sequencer, type DriveResult } from '../setup/longrun';
import type { Engine, Logger, ResumeInput } from './engine';
import type { Kickoff } from './envelope';
import { ParkedRun, RunRegistry } from './registry';

const TOOL_APPLY_FIX = 'apply_fix';
const TOOL_AWAIT_CI = 'await_ci';

/**
 * Per-run inputs the apply_fix tool needs. Owned by the Driver (keyed by session id)
 * and never model-controlled, so a misbehaving model cannot redirect which repo or
 * branch is edited.
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

/** Runs a Spec's CI-wait loop on ADK long-running suspend/resume. */
export class Driver {
  readonly reg = new RunRegistry();
  private readonly timeoutMs: number;
  private readonly runs = new Map<string, RunParams>();
  private seq = 0;
  private readonly lr: LongRunDriver;

  constructor(private readonly engine: Engine) {
    this.timeoutMs = engine.d.ciTimeoutMs;

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
    this.lr = new LongRunDriver(`fixflow-${engine.spec.name}`, 'fixer', fixer);
  }

  // --- tools -------------------------------------------------------------

  /**
   * Run one fix attempt for the calling session. The run params are looked up by session
   * id (Driver-owned), so the model's args cannot influence the target. Returns
   * `{ error }` on failure so the Sequencer's apply-error branch can conclude.
   */
  private async applyFixTool(ctx?: { sessionId?: string }): Promise<Record<string, unknown>> {
    try {
      const sid = ctx?.sessionId ?? '';
      const rp = this.runs.get(sid);
      if (!rp) {
        throw new Error(`apply_fix: no run params for session ${JSON.stringify(sid)}`);
      }
      const res = await this.engine.attemptOnce(rp);
      return { pr_number: res.pr.number, head_sha: res.headSha };
    } catch (err) {
      return { error: (err as Error).message };
    }
  }

  // --- lifecycle ---------------------------------------------------------

  /** Start a new suspended run: apply the fix, then park awaiting CI. */
  async kickoff(k: Kickoff): Promise<void> {
    const sid = this.newSessionId();
    this.runs.set(sid, {
      owner: k.owner(),
      repo: k.name(),
      fullRepo: k.repo,
      base: k.base,
      report: k.reportText(),
      feedback: '',
      newBranch: true,
    });
    let res: DriveResult;
    try {
      res = await this.lr.start(sid, 'Apply the fix and wait for CI.');
    } catch (err) {
      this.clear(sid);
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
    const run = this.reg.resolve(key);
    if (!run) {
      // Late, duplicate, raced with the timeout, or after a restart — nothing to do.
      this.log('info', 'resume: no parked run', { pr: key, conclusion: input.conclusion });
      return;
    }
    const link = pullUrl(input.fullRepo, input.prNumber);

    if (input.conclusion === 'success') {
      this.clear(run.sessionId);
      this.log('info', 'fix succeeded', { repo: input.fullRepo, pr: input.prNumber });
      await this.engine.notify(
        this.engine.spec.successTitle,
        `${input.fullRepo}: ${this.engine.spec.name} passed CI.`,
        link,
      );
      return;
    }

    // failure
    if (run.attempts >= this.engine.d.maxIter) {
      this.clear(run.sessionId);
      this.log('warn', 'fix exhausted attempts', {
        repo: input.fullRepo,
        pr: input.prNumber,
        attempts: run.attempts,
      });
      await this.engine.notify(
        this.engine.spec.reviewTitle,
        `${input.fullRepo}: after ${run.attempts} attempts the ${this.engine.spec.name} fix still fails CI. Please review.`,
        link,
      );
      return;
    }

    this.updateForRetry(run.sessionId, input.outputText);
    let res: DriveResult;
    try {
      res = await this.lr.resume(run.sessionId, run.callId, TOOL_AWAIT_CI, {
        conclusion: input.conclusion,
        output: input.outputText,
      });
    } catch (err) {
      this.clear(run.sessionId);
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
   * Fires (from the registry timer) when a parked run's CI never reports. Claims the
   * run, frees it, and asks for human review. Invoked detached (`void onTimeout`) by the
   * registry timer, so a notifier rejection is swallowed here rather than surfacing as a
   * process-level unhandledRejection.
   */
  async onTimeout(key: string): Promise<void> {
    const run = this.reg.resolve(key);
    if (!run) {
      return; // already resolved by a webhook
    }
    this.clear(run.sessionId);
    const [fullRepo, pr] = splitPrKey(key);
    const link = pullUrl(fullRepo, pr);
    this.log('warn', 'fix timed out waiting for CI', {
      repo: fullRepo,
      pr,
      timeoutMs: this.timeoutMs,
    });
    try {
      await this.engine.notify(
        this.engine.spec.reviewTitle,
        `${fullRepo}: the ${this.engine.spec.name} fix timed out after ${this.timeoutMs}ms waiting for CI. Please review.`,
        link,
      );
    } catch (err) {
      this.log('warn', 'timeout notification failed', { repo: fullRepo, pr, err: String(err) });
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
    const parked: ParkedRun = { sessionId: sid, callId: res.parkedCallId, attempts: attempt };
    this.reg.park(prKey(fullRepo, pr), parked, this.timeoutMs, (k) => this.onTimeout(k));
    this.log('info', 'fix applied; awaiting CI', { repo: fullRepo, pr, attempt });
  }

  /**
   * Free a run that errored before it could park on CI (a push/PR/analyze failure, not a
   * CI failure), notify a human, then throw. Without the notification an apply error would
   * only reach the dispatcher's logger and never the review channel — a fix that can't even
   * open its PR would vanish silently.
   */
  private async failApply(sid: string, fullRepo: string, reason: string): Promise<never> {
    this.clear(sid);
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
    this.seq += 1;
    return `run-${this.seq}`;
  }

  private updateForRetry(sid: string, feedback: string): void {
    const rp = this.runs.get(sid);
    if (rp) {
      rp.feedback = 'The previous attempt failed CI with:\n' + feedback;
      rp.newBranch = false;
    }
  }

  private clear(sid: string): void {
    this.runs.delete(sid);
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
