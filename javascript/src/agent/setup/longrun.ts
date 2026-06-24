/**
 * Generic ADK long-running suspend/resume plumbing.
 *
 * A {@link LongRunDriver} runs an agent until it parks on a long-running tool call
 * (or finishes), then resumes it with the real result; a {@link Sequencer} model
 * deterministically emits a fixed Action -> Wait tool sequence so all
 * retry/stop/timeout policy lives in the caller, not the model.
 *
 * A long-running tool that returns `null` parks the run (its function response is
 * withheld); feeding a function response for that call id back into the same session
 * resumes the model. Kept in `setup` because it touches genai (confined here by the
 * arch tests).
 */
import {
  BaseAgent,
  BaseLlm,
  type BaseSessionService,
  InMemorySessionService,
  type LlmRequest,
  type LlmResponse,
  Runner,
} from '@google/adk';
import type { Content, FunctionResponse } from '@google/genai';

import { assistantText, contentText } from './events';

/** The outcome of driving a long-running agent through one cycle. */
export interface DriveResult {
  /**
   * The id of the long-running call the agent suspended on, or "" when the run
   * finished instead of parking.
   */
  parkedCallId: string;
  /**
   * Maps each tool name to its most recent response this cycle. A tool whose
   * handler raised surfaces here under an "error" key.
   */
  toolResponses: Record<string, Record<string, unknown>>;
  /** The concatenated text of the agent's non-partial responses. */
  final: string;
}

/**
 * Drives an agent through ADK suspend/resume on a single in-memory session.
 *
 * All domain policy (what to apply, whether to retry, how long to wait) lives in
 * the caller; this type only knows how to run-to-park and resume-with-a-result.
 *
 * Concurrency: one driver (one {@link Runner} + one {@link BaseSessionService}) is
 * driven concurrently for distinct session ids (concurrent kickoffs and resumes). This
 * relies on `runAsync` and the session service being safe under concurrent invocations
 * on different sessions in the pinned adk-js — each invocation touches only its own
 * session, and JS's single-threaded event loop serializes the map mutations the
 * in-memory session service performs.
 *
 * The session service is injected so the fix loop can choose a durable backend (sqlite /
 * firestore) and have parked sessions survive a restart; it defaults to an in-memory
 * service, preserving today's behavior.
 */
export class LongRunDriver {
  private readonly sessionService: BaseSessionService;
  private readonly runner: Runner;

  constructor(
    private readonly appName: string,
    private readonly userId: string,
    root: BaseAgent,
    sessionService?: BaseSessionService,
  ) {
    this.sessionService = sessionService ?? new InMemorySessionService();
    this.runner = new Runner({
      appName,
      agent: root,
      sessionService: this.sessionService,
    });
  }

  /**
   * Seed a fresh invocation on `sessionId` and drive until the agent parks on a
   * long-running tool or finishes.
   */
  async start(sessionId: string, text: string): Promise<DriveResult> {
    await this.ensureSession(sessionId);
    return this.driveOnce(sessionId, { role: 'user', parts: [{ text }] });
  }

  /**
   * Feed the real result for a parked long-running call back into `sessionId` and
   * drive until the agent re-parks or finishes.
   */
  async resume(
    sessionId: string,
    callId: string,
    toolName: string,
    response: Record<string, unknown>,
  ): Promise<DriveResult> {
    const msg: Content = {
      role: 'user',
      parts: [{ functionResponse: { id: callId, name: toolName, response } }],
    };
    return this.driveOnce(sessionId, msg);
  }

  /**
   * Remove a session's stored history. Terminal cleanup so a completed (or abandoned) run
   * does not accumulate in a durable backend; a no-op for an already-absent session.
   */
  async deleteSession(sessionId: string): Promise<void> {
    await this.sessionService.deleteSession({
      appName: this.appName,
      userId: this.userId,
      sessionId,
    });
  }

  private async ensureSession(sessionId: string): Promise<void> {
    const existing = await this.sessionService.getSession({
      appName: this.appName,
      userId: this.userId,
      sessionId,
    });
    if (!existing) {
      await this.sessionService.createSession({
        appName: this.appName,
        userId: this.userId,
        sessionId,
      });
    }
  }

  private async driveOnce(sessionId: string, msg: Content): Promise<DriveResult> {
    const res: DriveResult = { parkedCallId: '', toolResponses: {}, final: '' };
    const parts: string[] = [];
    for await (const ev of this.runner.runAsync({
      userId: this.userId,
      sessionId,
      newMessage: msg,
    })) {
      if (ev.longRunningToolIds && ev.longRunningToolIds.length > 0) {
        res.parkedCallId = ev.longRunningToolIds[0]!;
      }
      if (!ev.content) {
        continue;
      }
      for (const p of ev.content.parts ?? []) {
        if (p.functionResponse?.name) {
          res.toolResponses[p.functionResponse.name] = {
            ...(p.functionResponse.response ?? {}),
          };
        }
      }
      if (!ev.partial) {
        parts.push(contentText(ev.content));
      }
    }
    res.final = parts.join('');
    return res;
  }
}

/** Decides whether a resumed wait result should trigger another attempt. */
export type RetryWhen = (response: Record<string, unknown>) => boolean;

/**
 * A deterministic `BaseLlm` that emits a fixed Action -> Wait tool sequence.
 *
 * Call `action` (a normal tool), then `wait` (a long-running tool that suspends the
 * run). When the run resumes with `wait`'s real result, `retryWhen` decides whether
 * to loop (call `action` again) or conclude. It carries no policy of its own: the
 * caller owns retry/stop/timeout and only resumes a parked run when it wants another
 * attempt.
 */
export class Sequencer extends BaseLlm {
  constructor(
    private readonly action: string,
    private readonly wait: string,
    private readonly retryWhen?: RetryWhen,
  ) {
    super({ model: `sequencer:${action}+${wait}` });
  }

  override async *generateContentAsync(req: LlmRequest): AsyncGenerator<LlmResponse, void> {
    yield this.decide(req.contents ?? []);
  }

  override async connect(): Promise<never> {
    throw new Error('sequencer does not support live connections');
  }

  /**
   * Choose the next step from the most recent function response in history:
   * - none yet                 -> call Action
   * - Action returned an error -> conclude (nothing to wait on)
   * - Action returned a result -> call Wait, forwarding the result as its args
   * - Wait result, retryWhen   -> call Action again
   * - Wait result, otherwise   -> conclude
   */
  private decide(contents: Content[]): LlmResponse {
    const last = lastFunctionResponse(contents);
    if (last === undefined) {
      return this.call(this.action, {}, contents);
    }
    if (last.name === this.action) {
      const resp = { ...(last.response ?? {}) };
      if ('error' in resp) {
        return sequencerText(`${this.action} failed: ${String(resp.error)}`);
      }
      return this.call(this.wait, resp, contents);
    }
    if (last.name === this.wait) {
      if (this.retryWhen && this.retryWhen({ ...(last.response ?? {}) })) {
        return this.call(this.action, {}, contents);
      }
      return sequencerText('done');
    }
    return sequencerText('done');
  }

  private call(name: string, args: Record<string, unknown>, contents: Content[]): LlmResponse {
    // Unique id per call so the flow correlates each long-running park independently
    // across retries within one session.
    const callId = `${name}_${countFunctionCalls(contents, name) + 1}`;
    return {
      content: { role: 'model', parts: [{ functionCall: { id: callId, name, args } }] },
      turnComplete: true,
    };
  }
}

function sequencerText(text: string): LlmResponse {
  return { content: assistantText(text), turnComplete: true };
}

function lastFunctionResponse(contents: Content[]): FunctionResponse | undefined {
  let last: FunctionResponse | undefined;
  for (const c of contents) {
    if (!c) {
      continue;
    }
    for (const p of c.parts ?? []) {
      if (p.functionResponse) {
        last = p.functionResponse;
      }
    }
  }
  return last;
}

function countFunctionCalls(contents: Content[], name: string): number {
  let n = 0;
  for (const c of contents) {
    if (!c) {
      continue;
    }
    for (const p of c.parts ?? []) {
      if (p.functionCall && p.functionCall.name === name) {
        n += 1;
      }
    }
  }
  return n;
}
