// Tests for the content + event helpers.
import { describe, expect, it } from 'vitest';

import {
  assistantText,
  contentText,
  lastText,
  stateString,
  textEvent,
  userText,
} from './events';

describe('events', () => {
  it('round-trips content text', () => {
    expect(contentText(userText('hi'))).toBe('hi');
    expect(contentText(assistantText('yo'))).toBe('yo');
    expect(contentText(null)).toBe('');
    expect(lastText([userText('a'), assistantText('b')])).toBe('b');
    expect(lastText([])).toBe('');
  });

  it('builds a text event with a state delta', () => {
    const ev = textEvent('fetcher', 'digest text', { 'commits:a/b': 'x' });
    expect(ev.author).toBe('fetcher');
    expect(contentText(ev.content)).toBe('digest text');
    expect(ev.actions.stateDelta).toEqual({ 'commits:a/b': 'x' });
  });

  it('builds a text event without a state delta', () => {
    const ev = textEvent('notifier', 'hello');
    expect(ev.actions.stateDelta).toEqual({});
  });

  it('reads string state safely', () => {
    expect(stateString({ k: 'v' }, 'k')).toBe('v');
    expect(stateString({ k: 3 }, 'k')).toBe('');
    expect(stateString({}, 'missing')).toBe('');
    expect(stateString(null, 'k')).toBe('');
    // State-like object exposing .get
    expect(stateString({ get: (key: string) => (key === 'k' ? 'v' : undefined) }, 'k')).toBe('v');
  });
});
