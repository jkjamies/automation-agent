/**
 * In-memory parked-run registry — the fix-loop spine.
 *
 * Tracks suspended fix runs keyed by PR. Exactly one of {CI webhook, timeout timer}
 * ever resolves a given run: {@link RunRegistry.resolve} removes the entry, so
 * late/duplicate deliveries find nothing and no-op. The registry IS the in-flight
 * record — no DB, no PR scan. Parked runs live only in memory.
 *
 * `resolve` is naturally atomic: the single event loop never preempts between the map
 * lookup and delete. The timeout handle is stored on the ParkedRun so resolve can
 * cancel it.
 */

/**
 * One suspended fix run awaiting its CI result. `sessionId` + `callId` are what a
 * resume needs to feed the CI outcome back into the parked run.
 */
export interface ParkedRun {
  sessionId: string;
  callId: string;
  attempts: number;
  timer?: ReturnType<typeof setTimeout>;
}

/** Tracks parked runs in memory, keyed by PR. */
export class RunRegistry {
  private readonly runs = new Map<string, ParkedRun>();

  /**
   * Record a parked run for `prKey` and arm its timeout. `onTimeout` fires once if the
   * run is still parked when `timeoutMs` elapses; it must call {@link resolve} to claim
   * the run (and loses the claim if a webhook got there first). A prior parking for the
   * same key (e.g. a retry re-park) is replaced and its timer cancelled.
   */
  park(
    prKey: string,
    run: ParkedRun,
    timeoutMs: number,
    onTimeout: (prKey: string) => void | Promise<void>,
  ): void {
    const old = this.runs.get(prKey);
    if (old?.timer) {
      clearTimeout(old.timer);
    }
    run.timer = setTimeout(() => {
      void onTimeout(prKey);
    }, timeoutMs);
    // Don't let the pending timer keep the process alive.
    run.timer.unref?.();
    this.runs.set(prKey, run);
  }

  /**
   * Atomically claim and remove the parked run for `prKey`, cancelling its timer.
   * Returns the run for the single winner; undefined for late/duplicate/unknown callers.
   */
  resolve(prKey: string): ParkedRun | undefined {
    const run = this.runs.get(prKey);
    if (!run) {
      return undefined;
    }
    this.runs.delete(prKey);
    if (run.timer) {
      clearTimeout(run.timer);
    }
    return run;
  }

  /** Report the number of currently parked runs. */
  size(): number {
    return this.runs.size;
  }
}
