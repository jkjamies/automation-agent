// Tests for the in-process execution transport.
import { afterEach, describe, expect, it, vi } from 'vitest';

import { type Envelope, Kind, newEnvelope } from '../ingest/envelope';
import { DEFAULT_MAX_CONCURRENT, InProcess } from './inprocess';

function env(kind: Kind = Kind.Lint, payload = 'x'): Envelope {
  return newEnvelope(kind, 'webhook:/lint', Buffer.from(payload), new Date(0));
}

/** A promise plus its resolver, for hand-driving test timing. */
function deferred(): { promise: Promise<void>; resolve: () => void } {
  let resolve!: () => void;
  const promise = new Promise<void>((r) => {
    resolve = r;
  });
  return { promise, resolve };
}

/** Yield once to the macrotask queue so detached promises make progress. */
function tick(): Promise<void> {
  return new Promise((r) => setImmediate(r));
}

describe('InProcess', () => {
  afterEach(() => {
    vi.useRealTimers();
  });

  it('dispatches an enqueued envelope', async () => {
    const got: Envelope[] = [];
    const done = deferred();
    const p = new InProcess(async (e) => {
      got.push(e);
      done.resolve();
    }, null, 4);

    await p.enqueue(env(Kind.Lint));
    await done.promise;
    await p.close();
    expect(got[0]?.kind).toBe(Kind.Lint);
  });

  it('swallows a dispatch error rather than rejecting enqueue', async () => {
    // A dispatch error is logged, not raised (the webhook response has already gone out), so
    // enqueue still resolves.
    const done = deferred();
    const p = new InProcess(async () => {
      done.resolve();
      throw new Error('boom');
    }, null, 1);

    await expect(p.enqueue(env(Kind.CI))).resolves.toBeUndefined();
    await done.promise;
    await p.close();
  });

  it('falls back to the default concurrency for a non-positive max', async () => {
    const done = deferred();
    const p = new InProcess(async () => done.resolve(), null, 0);
    expect((p as unknown as { maxConcurrent: number }).maxConcurrent).toBe(DEFAULT_MAX_CONCURRENT);
    await p.enqueue(env());
    await done.promise;
    await p.close();
  });

  it('drains an in-flight dispatch before close() resolves', async () => {
    const release = deferred();
    let finished = false;
    const p = new InProcess(async () => {
      await release.promise;
      finished = true;
    }, null, 1);
    await p.enqueue(env(Kind.CronDaily));

    let closed = false;
    const closing = p.close().then(() => {
      closed = true;
    });
    await tick();
    expect(closed).toBe(false); // close() is still waiting on the blocked dispatch
    release.resolve();
    await closing;
    expect(finished).toBe(true);
  });

  it('rejects enqueue after close', async () => {
    let ran = false;
    const p = new InProcess(async () => {
      ran = true;
    }, null, 1);
    await p.close();
    await expect(p.enqueue(env(Kind.CI))).rejects.toThrow(/closed/);
    expect(ran).toBe(false);
  });

  it('rejects an enqueue that was parked when close() began (recheck after acquire)', async () => {
    // An enqueue parked on a permit when close() begins must back out (release its slot and
    // throw) once it acquires, rather than spawn a dispatch the drain has already snapshotted
    // past — the recheck-after-acquire guard (mirrors Go's second select on the closed channel).
    const release = deferred();
    const started = deferred();
    const p = new InProcess(async () => {
      started.resolve();
      await release.promise;
    }, null, 1);

    await p.enqueue(env()); // occupies the only slot
    await started.promise; // dispatch #1 is running, slot held
    // second passes its initial closed-check (still open), then parks on acquire().
    const second = p.enqueue(env());
    const secondAssertion = expect(second).rejects.toThrow(/closed/);
    // Real shutdown: close() marks closed, then drains the in-flight dispatch.
    const closing = p.close();
    await tick();
    release.resolve(); // dispatch #1 finishes -> slot frees -> second acquires, rechecks closed
    await secondAssertion;
    await closing;
  });

  it('applies backpressure when the pool is full', async () => {
    // With the pool full, a second enqueue blocks until a slot frees.
    const release = deferred();
    const p = new InProcess(async () => {
      await release.promise;
    }, null, 1);
    await p.enqueue(env()); // occupies the only slot

    let secondDone = false;
    const second = p.enqueue(env()).then(() => {
      secondDone = true;
    });
    await tick();
    expect(secondDone).toBe(false); // blocked on the permit
    release.resolve();
    await second;
    expect(secondDone).toBe(true);
    await p.close();
  });

  it('does not cancel a still-running dispatch when the drain times out', async () => {
    // On drain timeout, close() only stops waiting — it must NOT abort the still-running
    // dispatch (JS cannot cancel a promise; this matches Go's Close letting in-flight goroutines
    // run to completion).
    vi.useFakeTimers();
    const release = deferred();
    let finished = false;
    const p = new InProcess(async () => {
      await release.promise;
      finished = true;
    }, null, 1);
    await p.enqueue(env());

    const closing = p.close();
    await vi.advanceTimersByTimeAsync(15_000); // exceed DRAIN_TIMEOUT_MS
    await closing;
    expect(finished).toBe(false); // still running, not cancelled

    release.resolve(); // it now runs to completion on its own
    await vi.runAllTimersAsync();
    await release.promise;
    expect(finished).toBe(true);
  });
});
