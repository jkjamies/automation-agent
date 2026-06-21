// Tests for the in-memory runner drive helpers.
import { BaseAgent, createEvent, createEventActions, type Event, LlmAgent } from '@google/adk';
import { describe, expect, it } from 'vitest';

import { FakeLlm } from '../../testutil/fakes';
import { drive, driveCollectState, driveText, newRunner } from './runner';

// A minimal custom agent that emits two state deltas.
class Writer extends BaseAgent {
  protected override async *runAsyncImpl(): AsyncGenerator<Event, void> {
    yield createEvent({ author: this.name, actions: createEventActions({ stateDelta: { a: 1 } }) });
    yield createEvent({ author: this.name, actions: createEventActions({ stateDelta: { b: 2 } }) });
  }
  protected override async *runLiveImpl(): AsyncGenerator<Event, void> {
    // not used
  }
}

describe('runner', () => {
  it('driveText returns the agent final text', async () => {
    const agent = new LlmAgent({ name: 'echo', model: new FakeLlm('final answer'), instruction: 'x' });
    const runner = newRunner('t', agent);
    const out = await driveText(runner, 'u', 's1', 'hello');
    expect(out).toContain('final answer');
  });

  it('drive drains events without throwing', async () => {
    const agent = new LlmAgent({ name: 'echo2', model: new FakeLlm('ok'), instruction: 'x' });
    const runner = newRunner('t', agent);
    await expect(drive(runner, 'u', 's2', 'hi')).resolves.toBeUndefined();
  });

  it('driveCollectState accumulates state deltas', async () => {
    const runner = newRunner('t', new Writer({ name: 'w' }));
    const state = await driveCollectState(runner, 'u', 's3', 'go');
    expect(state).toEqual({ a: 1, b: 2 });
  });
});
