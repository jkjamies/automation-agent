/**
 * In-memory runner helpers for driving workflow agents locally and in tests.
 *
 * Each helper iterates `for await` over ADK's `Runner.runAsync`, draining or
 * collecting events as needed.
 */
import { type BaseAgent, InMemorySessionService, Runner } from '@google/adk';

import { contentText, userText } from './events';
import { STREAMING_RUN_CONFIG } from './runconfig';

/** Build an in-memory runner rooted at `root`. */
export function newRunner(appName: string, root: BaseAgent): Runner {
  return new Runner({
    appName,
    agent: root,
    sessionService: new InMemorySessionService(),
  });
}

/**
 * Ensure a session exists, then run the agent for a single input, draining events.
 * Side-effecting agents (e.g. a notifier) perform their work as they run.
 */
export async function drive(
  runner: Runner,
  userId: string,
  sessionId: string,
  text: string,
): Promise<void> {
  await ensureSession(runner, userId, sessionId);
  for await (const _ev of runner.runAsync({
    userId,
    sessionId,
    newMessage: userText(text),
    runConfig: STREAMING_RUN_CONFIG,
  })) {
    // drain
  }
}

/**
 * Run the agent and return the concatenated text of its non-partial responses.
 * For a tool-using agent this is the final answer after any tool calls
 * (intermediate function-call/response events carry no text).
 */
export async function driveText(
  runner: Runner,
  userId: string,
  sessionId: string,
  text: string,
): Promise<string> {
  await ensureSession(runner, userId, sessionId);
  const parts: string[] = [];
  for await (const ev of runner.runAsync({
    userId,
    sessionId,
    newMessage: userText(text),
    runConfig: STREAMING_RUN_CONFIG,
  })) {
    if (ev.content && !ev.partial) {
      parts.push(contentText(ev.content));
    }
  }
  return parts.join('');
}

/**
 * Run the agent and accumulate every emitted state delta into one map.
 * Useful for fan-out workflows where parallel sub-agents each write a distinct
 * state key the caller needs to read back.
 */
export async function driveCollectState(
  runner: Runner,
  userId: string,
  sessionId: string,
  text: string,
): Promise<Record<string, unknown>> {
  await ensureSession(runner, userId, sessionId);
  const state: Record<string, unknown> = {};
  for await (const ev of runner.runAsync({
    userId,
    sessionId,
    newMessage: userText(text),
    runConfig: STREAMING_RUN_CONFIG,
  })) {
    const delta = ev.actions?.stateDelta;
    if (delta) {
      Object.assign(state, delta);
    }
  }
  return state;
}

async function ensureSession(runner: Runner, userId: string, sessionId: string): Promise<void> {
  const svc = runner.sessionService;
  const existing = await svc.getSession({ appName: runner.appName, userId, sessionId });
  if (!existing) {
    await svc.createSession({ appName: runner.appName, userId, sessionId });
  }
}
