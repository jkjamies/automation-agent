/**
 * Small genai/ADK content + event helpers used by code agents.
 *
 * Code agents use {@link textEvent} to emit model-authored output and write workflow
 * state in one shot.
 */
import { createEvent, createEventActions, type Event } from '@google/adk';
import type { Content } from '@google/genai';

export const ROLE_USER = 'user';
export const ROLE_MODEL = 'model';

/** Build a user-role content message from plain text (seeds an invocation). */
export function userText(text: string): Content {
  return { role: ROLE_USER, parts: [{ text }] };
}

/** Build a model-role content message from plain text. */
export function assistantText(text: string): Content {
  return { role: ROLE_MODEL, parts: [{ text }] };
}

/** Concatenate the text parts of a content (null-safe). */
export function contentText(content: Content | null | undefined): string {
  if (!content || !content.parts) {
    return '';
  }
  return content.parts.map((p) => p.text ?? '').join('');
}

/** Return the concatenated text of the final content in a list, or "". */
export function lastText(contents: Content[]): string {
  if (contents.length === 0) {
    return '';
  }
  return contentText(contents[contents.length - 1]);
}

/** Build an Event carrying model-authored text, optionally with a state delta. */
export function textEvent(
  author: string,
  text: string,
  state?: Record<string, unknown>,
): Event {
  const actions =
    state && Object.keys(state).length > 0
      ? createEventActions({ stateDelta: { ...state } })
      : createEventActions();
  return createEvent({ author, content: assistantText(text), actions });
}

/**
 * Return the string value at `key`, or "" if absent or not a string.
 *
 * `state` is any mapping-like session state: a plain object (`session.state`) or
 * an ADK `State` instance (which exposes `.get`).
 */
export function stateString(state: unknown, key: string): string {
  if (state == null) {
    return '';
  }
  let value: unknown;
  const maybeGet = (state as { get?: (k: string) => unknown }).get;
  if (typeof maybeGet === 'function') {
    value = maybeGet.call(state, key);
  } else {
    value = (state as Record<string, unknown>)[key];
  }
  return typeof value === 'string' ? value : '';
}
