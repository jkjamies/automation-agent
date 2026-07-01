/** The Cloud Tasks execution transport — the production backend. */

import { CloudTasksClient, protos } from '@google-cloud/tasks';

import { type Envelope, encode } from '../ingest/envelope';
import { inject } from '../obs/propagation';
import { type EnqueueOptions, type Transport } from './transport';

type ICreateTaskRequest = protos.google.cloud.tasks.v2.ICreateTaskRequest;
type ITask = protos.google.cloud.tasks.v2.ITask;

/**
 * MAX_TASK_BYTES is the Cloud Tasks size limit for an HTTP-target task (1 MiB; verify against
 * current quota docs). enqueue refuses an envelope whose encoded body exceeds it rather than
 * letting Cloud Tasks reject the create call opaquely (spec §9). Today's payloads are metadata
 * well under this (PR diffs are fetched later via the API, not carried in the webhook body); if
 * a future payload could exceed it, the fallback is store-in-Firestore + enqueue a reference —
 * noted in the spec, not built here.
 */
export const MAX_TASK_BYTES = 1 << 20;

/**
 * The slice of the Cloud Tasks client this backend uses, isolated so task-building can be
 * unit-tested against a fake without a live gRPC connection.
 */
export interface Submitter {
  createTask(request: ICreateTaskRequest): Promise<unknown>;
  close(): Promise<void>;
}

/**
 * Enqueues each envelope as a Cloud Tasks HTTP-target task pointed at `/internal/dispatch` —
 * the production backend.
 *
 * The queue gives durable retry with backoff (a task survives the instance being reclaimed
 * mid-run and is redelivered) and rate limiting (the queue's max-concurrent-dispatches
 * replaces the in-process semaphore), and the worker runs in-request so CPU stays allocated
 * for the whole compute.
 */
export class CloudTasks implements Transport {
  private readonly client: Submitter;
  private readonly queuePath: string;
  private readonly dispatchUrl: string;
  private readonly token: string;
  // Explicit per-task dispatch deadline in milliseconds. The HTTP-target default is only 10m,
  // so a longer workflow would be cancelled mid-run and retried (duplicating side effects).
  private readonly deadlineMs: number;
  private readonly now: () => Date;

  constructor(opts: {
    client: Submitter;
    queuePath: string;
    dispatchUrl: string;
    token: string;
    deadlineMs?: number;
    now?: () => Date;
  }) {
    this.client = opts.client;
    this.queuePath = opts.queuePath;
    this.dispatchUrl = opts.dispatchUrl;
    this.token = opts.token;
    this.deadlineMs = opts.deadlineMs ?? 0;
    this.now = opts.now ?? (() => new Date());
  }

  /**
   * Build and submit a task carrying the JSON-encoded envelope as its body and the
   * INTERNAL_TOKEN as a Bearer header. `name` sets the task name (Cloud Tasks dedup); `delayMs`
   * sets the schedule time.
   *
   * @throws Error if the envelope's kind is unknown (rejected by {@link encode}), the encoded
   *   body exceeds the Cloud Tasks task-size limit, or the create call fails (a transient
   *   failure the caller surfaces as a 500 so the queue retries).
   */
  async enqueue(e: Envelope, opts: EnqueueOptions = {}): Promise<void> {
    const body = encode(e);
    if (body.length > MAX_TASK_BYTES) {
      throw new Error(
        `tasks: envelope is ${body.length} bytes, over the ${MAX_TASK_BYTES}-byte Cloud Tasks task limit`,
      );
    }

    const headers: Record<string, string> = { 'Content-Type': 'application/json' };
    if (this.token !== '') {
      headers.Authorization = 'Bearer ' + this.token;
    }
    // Inject the W3C trace context so the /internal/dispatch worker continues the ingress trace
    // (a `traceparent` header on the task, not in the envelope JSON — that is a versioned wire
    // contract). A no-op when tracing is disabled, so no header is added.
    inject(headers);
    const task: ITask = {
      httpRequest: { httpMethod: 'POST', url: this.dispatchUrl, headers, body },
    };
    // Set the dispatch deadline explicitly (the HTTP-target default is only 10m). Skipped when
    // unset (zero) so the queue default applies — production always supplies it.
    if (this.deadlineMs > 0) {
      task.dispatchDeadline = durationFromMs(this.deadlineMs);
    }
    if (opts.name) {
      task.name = this.queuePath + '/tasks/' + opts.name;
    }
    if (opts.delayMs && opts.delayMs > 0) {
      task.scheduleTime = timestampFromMs(this.now().getTime() + opts.delayMs);
    }

    try {
      await this.client.createTask({ parent: this.queuePath, task });
    } catch (err) {
      throw new Error(`tasks: create task: ${(err as Error).message}`);
    }
  }

  /** Release the underlying Cloud Tasks client. */
  async close(): Promise<void> {
    await this.client.close();
  }
}

/**
 * Open a Cloud Tasks client and target the queue
 * `projects/<project>/locations/<location>/queues/<queue>`.
 *
 * `dispatchUrl` is the full URL of the `/internal/dispatch` worker; `token` is the static
 * INTERNAL_TOKEN the task carries as a Bearer header (the same auth that endpoint already
 * enforces). `deadlineMs` is the explicit per-task dispatch deadline (config validated to
 * Cloud Tasks' 15s..30m range). {@link CloudTasks.close} releases the client.
 */
export function newCloudTasks(
  project: string,
  location: string,
  queue: string,
  dispatchUrl: string,
  token: string,
  deadlineMs: number,
): CloudTasks {
  const client = new CloudTasksClient();
  return new CloudTasks({
    client,
    queuePath: client.queuePath(project, location, queue),
    dispatchUrl,
    token,
    deadlineMs,
  });
}

/** A google.protobuf.Duration from a millisecond count. */
function durationFromMs(ms: number): protos.google.protobuf.IDuration {
  return { seconds: Math.floor(ms / 1000), nanos: (ms % 1000) * 1_000_000 };
}

/** A google.protobuf.Timestamp from an epoch-millisecond instant. */
function timestampFromMs(epochMs: number): protos.google.protobuf.ITimestamp {
  return { seconds: Math.floor(epochMs / 1000), nanos: (epochMs % 1000) * 1_000_000 };
}
