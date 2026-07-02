/**
 * The debounce/coalesce transport hints for a synchronize review.
 *
 * Rapid pushes to one PR are collapsed so only the latest SHA is reviewed: a `synchronize` review
 * is enqueued with a debounce delay under a per-PR-per-window Cloud Tasks dedup name, so a burst of
 * pushes collapses to one delayed task. `opened`/`reopened`/`ready_for_review` enqueue immediately
 * (a human is waiting on the first review). Coalescing is a workflow concern, so it lives here
 * rather than in the transport (which stays dumb about PRs and SHAs).
 */

import { type PullRequestEvent, parsePullRequestEvent } from '../../githubapi/client';
import { type Envelope, Kind } from '../../ingest/envelope';
import type { EnqueueOptions } from '../../tasks/transport';

/**
 * Nanoseconds between the proleptic-calendar zero instant (Jan 1, year 1 UTC) and the Unix epoch.
 * The debounce window is floored relative to that zero instant, not the Unix epoch, so the bucket
 * carried in the dedup name must be computed with the same origin to stay byte-identical across
 * every port (the name is a cross-port external contract).
 */
const UNIX_TO_INTERNAL_NS = 62135596800n * 1_000_000_000n;

const NS_PER_MS = 1_000_000n;

/**
 * Return the transport hints for a review envelope so rapid pushes coalesce. A pull_request
 * "synchronize" (a new push to an open PR) is enqueued under a per-PR dedup name with a debounce
 * delay, so a burst of pushes collapses to one delayed task that reviews the latest SHA; the
 * worker's staleness check then enforces newest-wins. Any non-review kind, an unparseable payload,
 * or a non-positive debounce yields no options (immediate enqueue). Only the Cloud Tasks backend
 * honors the hints; the in-process backend ignores them.
 */
export function enqueueOptions(e: Envelope, debounceMs: number): EnqueueOptions {
  if (e.kind !== Kind.Review || debounceMs <= 0) {
    return {};
  }
  let ev: PullRequestEvent;
  try {
    ev = parsePullRequestEvent(e.payload);
  } catch {
    return {};
  }
  if (ev.action !== 'synchronize') {
    return {};
  }
  const bucket = truncateToWindow(e.receivedAt, debounceMs);
  return { name: coalesceKey(ev, bucket), delayMs: debounceMs };
}

/**
 * The per-PR-per-window Cloud Tasks dedup name. Identically-named tasks collapse to one, so a burst
 * of pushes within a debounce window coalesces to a single review of the latest SHA.
 *
 * The name carries a time bucket (the receipt time floored to the debounce window) because Cloud
 * Tasks keeps a task name reserved for ~1h after the task completes or is deleted: a fixed per-PR
 * name would make a push that lands minutes after the previous review collide with the reserved
 * name and silently drop the new review. Bucketing gives each window a fresh name.
 *
 * The repo full name is base64url-encoded so the name is both valid in the Cloud Tasks charset
 * ([A-Za-z0-9_-]) and lossless: a naive replace-invalid-with-'-' would collapse distinct repos
 * (e.g. "acme/web.api" and "acme/web-api") to the same name and silently drop one PR's review.
 */
export function coalesceKey(ev: PullRequestEvent, bucketUnixNs: bigint): string {
  const encoded = Buffer.from(ev.repoFullName, 'utf-8').toString('base64url');
  return `review-${encoded}-${ev.number}-${bucketUnixNs}`;
}

/**
 * Floor `at` to a multiple of `debounceMs` measured from the proleptic-calendar zero instant (see
 * {@link UNIX_TO_INTERNAL_NS}), returning the result as Unix nanoseconds. Computing the window
 * origin this way keeps the bucket byte-identical across every port.
 */
function truncateToWindow(at: Date, debounceMs: number): bigint {
  const unixNs = BigInt(at.getTime()) * NS_PER_MS;
  const windowNs = BigInt(debounceMs) * NS_PER_MS;
  if (windowNs <= 0n) {
    return unixNs;
  }
  return unixNs - ((unixNs + UNIX_TO_INTERNAL_NS) % windowNs);
}
