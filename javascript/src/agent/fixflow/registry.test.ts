// Tests for the parked-run registry: single-winner resolve, re-park, timeout firing.
import { describe, expect, it } from 'vitest';

import { RunRegistry } from './registry';

const noTimeout = (): void => {};
const sleep = (ms: number): Promise<void> => new Promise((r) => setTimeout(r, ms));

describe('RunRegistry', () => {
  it('resolves a parked run exactly once', () => {
    const r = new RunRegistry();
    r.park('o/r#1', { sessionId: 's', callId: 'c', attempts: 0 }, 3_600_000, noTimeout);
    expect(r.size()).toBe(1);

    const run = r.resolve('o/r#1');
    expect(run?.callId).toBe('c');
    expect(r.resolve('o/r#1')).toBeUndefined(); // already claimed
    expect(r.size()).toBe(0);
  });

  it('no-ops on a late/unknown resolve', () => {
    const r = new RunRegistry();
    expect(r.resolve('never/parked#9')).toBeUndefined();
  });

  it('fires the timeout, which claims the run', async () => {
    const r = new RunRegistry();
    const claimed: boolean[] = [];
    r.park('o/r#2', { sessionId: 's', callId: 'c', attempts: 0 }, 50, (pr) => {
      claimed.push(r.resolve(pr) !== undefined);
    });
    await sleep(150);
    expect(claimed).toEqual([true]);
    expect(r.size()).toBe(0);
    expect(r.resolve('o/r#2')).toBeUndefined();
  });

  it('cancels the timer when resolved before timeout', async () => {
    const r = new RunRegistry();
    const fired: string[] = [];
    r.park('o/r#5', { sessionId: 's', callId: 'c', attempts: 0 }, 50, (pr) => {
      fired.push(pr);
    });
    expect(r.resolve('o/r#5')).toBeDefined();
    await sleep(150);
    expect(fired).toEqual([]);
  });

  it('replaces a prior parking on re-park', () => {
    const r = new RunRegistry();
    r.park('o/r#4', { sessionId: 's', callId: 'c1', attempts: 1 }, 3_600_000, noTimeout);
    r.park('o/r#4', { sessionId: 's', callId: 'c2', attempts: 2 }, 3_600_000, noTimeout);
    expect(r.size()).toBe(1);
    const run = r.resolve('o/r#4');
    expect(run?.callId).toBe('c2');
    expect(run?.attempts).toBe(2);
  });
});
