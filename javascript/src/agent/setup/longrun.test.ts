// Tests for the suspend/resume Sequencer + LongRunDriver.
//
// The apply tool returns {error: ...} itself to exercise the sequencer's error branch,
// the same convention fixflow's real applyFix tool uses.
import { FunctionTool, LlmAgent, LongRunningFunctionTool } from '@google/adk';
import type { Content } from '@google/genai';
import { describe, expect, it } from 'vitest';

import { LongRunDriver, Sequencer } from './longrun';

class Tools {
  calls = 0;
  fail = false;
  apply(): Record<string, unknown> {
    this.calls += 1;
    return this.fail ? { error: 'apply boom' } : { pr_number: 7, head_sha: 'abc' };
  }
  awaitCi(): null {
    return null; // long-running: returning null parks the run
  }
}

function newDriver(tools: Tools): LongRunDriver {
  const seq = new Sequencer('apply', 'await_ci', (r) => String(r.conclusion) === 'failure');
  const agent = new LlmAgent({
    name: 'lr',
    model: seq,
    instruction: 'apply then await',
    tools: [
      new FunctionTool({ name: 'apply', description: 'apply', execute: () => tools.apply() }),
      new LongRunningFunctionTool({
        name: 'await_ci',
        description: 'await CI',
        execute: () => tools.awaitCi(),
      }),
    ],
  });
  return new LongRunDriver('lr-app', 'u', agent);
}

describe('LongRunDriver', () => {
  it('loops apply -> await -> resume until success', async () => {
    const tools = new Tools();
    const d = newDriver(tools);

    const start = await d.start('s1', 'go');
    expect(start.parkedCallId).not.toBe('');
    expect(String(start.toolResponses.apply!.pr_number)).toBe('7');
    expect(tools.calls).toBe(1);

    // CI failed -> resume re-applies and re-parks with a fresh call id.
    const retry = await d.resume('s1', start.parkedCallId, 'await_ci', { conclusion: 'failure' });
    expect(retry.parkedCallId).not.toBe('');
    expect(retry.parkedCallId).not.toBe(start.parkedCallId);
    expect(tools.calls).toBe(2);

    // CI passed -> resume concludes without re-parking or re-applying.
    const done = await d.resume('s1', retry.parkedCallId, 'await_ci', { conclusion: 'success' });
    expect(done.parkedCallId).toBe('');
    expect(tools.calls).toBe(2);
    expect(done.final).toContain('done');
  });

  it('does not park when apply errors', async () => {
    const tools = new Tools();
    tools.fail = true;
    const d = newDriver(tools);

    const res = await d.start('s1', 'go');
    expect(res.parkedCallId).toBe('');
    expect(res.toolResponses.apply!.error).toBeDefined();
    expect(res.final).toContain('failed');
  });

  it('ignores a late/duplicate resume on a concluded run', async () => {
    const tools = new Tools();
    const d = newDriver(tools);
    const start = await d.start('s1', 'go');

    // timeout concludes the run (retryWhen is false for "timeout").
    const timedOut = await d.resume('s1', start.parkedCallId, 'await_ci', { conclusion: 'timeout' });
    expect(timedOut.parkedCallId).toBe('');

    // late webhook replays the same (now stale) call id -> must not re-park.
    const late = await d.resume('s1', start.parkedCallId, 'await_ci', { conclusion: 'success' });
    expect(late.parkedCallId).toBe('');
  });
});

describe('Sequencer stopWhen', () => {
  it('concludes without calling Wait when the Action result satisfies stopWhen', async () => {
    let applied = 0;
    let awaited = 0;
    const seq = new Sequencer(
      'apply',
      'await_ci',
      (r) => String(r.conclusion) === 'failure',
      (r) => r.clean === true,
    );
    const agent = new LlmAgent({
      name: 'lr',
      model: seq,
      instruction: 'apply then await',
      tools: [
        new FunctionTool({
          name: 'apply',
          description: 'apply',
          execute: () => {
            applied += 1;
            return { clean: true };
          },
        }),
        new LongRunningFunctionTool({
          name: 'await_ci',
          description: 'await CI',
          execute: () => {
            awaited += 1;
            return null;
          },
        }),
      ],
    });
    const d = new LongRunDriver('lr-app', 'u', agent);

    const res = await d.start('s1', 'go');
    expect(res.parkedCallId).toBe(''); // a clean (stopWhen) apply must not park
    expect(res.toolResponses.apply!.clean).toBe(true);
    expect(applied).toBe(1);
    expect(awaited).toBe(0);
  });
});

describe('Sequencer.decide', () => {
  const seq = new Sequencer('apply', 'await_ci', (r) => String(r.conclusion) === 'failure');

  function decide(contents: Content[]): { name: string; text: string } {
    const resp = (seq as unknown as { decide(c: Content[]): { content: Content } }).decide(contents);
    let text = '';
    for (const p of resp.content.parts ?? []) {
      if (p.functionCall) return { name: p.functionCall.name ?? '', text: '' };
      if (p.text) text += p.text;
    }
    return { name: '', text };
  }
  const resp = (name: string, body: Record<string, unknown>): Content => ({
    parts: [{ functionResponse: { name, response: body } }],
  });

  it('chooses the next step from history', () => {
    expect(decide([]).name).toBe('apply');
    expect(decide([resp('apply', { pr_number: 7 })]).name).toBe('await_ci');
    expect(decide([resp('apply', { error: 'x' })]).text).toContain('failed');
    expect(decide([resp('await_ci', { conclusion: 'failure' })]).name).toBe('apply');
    const ok = decide([resp('await_ci', { conclusion: 'success' })]);
    expect(ok.name).toBe('');
    expect(ok.text).not.toBe('');
  });
});
