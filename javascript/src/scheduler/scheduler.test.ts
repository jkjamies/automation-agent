// Tests for the cron scheduler.
import { describe, expect, it, vi } from 'vitest';

import { type Envelope, Kind } from '../ingest/envelope';
import { Scheduler } from './scheduler';

describe('scheduler', () => {
  it('adds valid specs and rejects an invalid one', () => {
    const s = new Scheduler(() => {});
    s.add('0 9 * * *', Kind.CronDaily);
    s.add('0 9 * * 1', Kind.CronWeekly);
    expect(s.entries()).toBe(2);
    expect(() => s.add('not a cron spec', Kind.CronDaily)).toThrow();
  });

  it('emits an envelope on trigger with an injected clock', () => {
    const captured: Envelope[] = [];
    const fixed = new Date(1718870400 * 1000);
    const s = new Scheduler((e) => captured.push(e), () => fixed);

    // trigger is the directly unit-testable emit path, separated from the cron
    // closure exactly so it can be exercised without waiting.
    (s as unknown as { trigger(k: Kind): void }).trigger(Kind.CronWeekly);

    expect(captured).toHaveLength(1);
    const got = captured[0]!;
    expect(got.kind).toBe(Kind.CronWeekly);
    expect(got.source).toBe('scheduler');
    expect(got.payload.length).toBe(0);
    expect(got.receivedAt).toBe(fixed);
  });

  // Uses a real 6-field every-second cron so the assertion is on observable firing,
  // not on croner's internals under fake timers.
  it('fires a scheduled job after start and stops cleanly', async () => {
    const captured: Envelope[] = [];
    const s = new Scheduler((e) => captured.push(e));
    s.add('* * * * * *', Kind.CronDaily); // every second
    s.start();
    try {
      await vi.waitFor(() => expect(captured.length).toBeGreaterThanOrEqual(1), {
        timeout: 3000,
        interval: 50,
      });
      expect(captured[0]!.kind).toBe(Kind.CronDaily);
    } finally {
      s.stop();
    }
  });

  it('does not fire before start (jobs are created paused)', async () => {
    const captured: Envelope[] = [];
    const s = new Scheduler((e) => captured.push(e));
    s.add('* * * * * *', Kind.CronDaily);
    // Never call start(); give a real second to prove nothing fires.
    await new Promise((r) => setTimeout(r, 1200));
    expect(captured).toHaveLength(0);
    s.stop();
  });
});
