// Tests for the Cloud Tasks execution transport (task-building, exercised against a fake
// submitter so no live gRPC client is needed).
import { protos } from '@google-cloud/tasks';
import { context, propagation, trace } from '@opentelemetry/api';
import { InMemorySpanExporter } from '@opentelemetry/sdk-trace-base';
import { afterEach, describe, expect, it } from 'vitest';

import { decode, encode, type Envelope, Kind, newEnvelope } from '../ingest/envelope';
import { TracesExporter, install } from '../obs/obs';
import { CloudTasks, MAX_TASK_BYTES, type Submitter } from './cloudtasks';

type ICreateTaskRequest = protos.google.cloud.tasks.v2.ICreateTaskRequest;

function env(kind: Kind = Kind.Lint, payload = 'x'): Envelope {
  return newEnvelope(kind, 'webhook:/lint', Buffer.from(payload), new Date(0));
}

/** Records the last CreateTaskRequest and returns a configurable error. */
class FakeSubmitter implements Submitter {
  request: ICreateTaskRequest | null = null;
  closed = false;
  constructor(private readonly err: Error | null = null) {}

  async createTask(request: ICreateTaskRequest): Promise<unknown> {
    this.request = request;
    if (this.err !== null) {
      throw this.err;
    }
    return [request.task];
  }

  async close(): Promise<void> {
    this.closed = true;
  }
}

/** Build a CloudTasks over a fake submitter with a fixed clock, so task building is exercised
 * without a live gRPC client. */
function newCT(f: FakeSubmitter, token: string): CloudTasks {
  return new CloudTasks({
    client: f,
    queuePath: 'projects/p/locations/l/queues/q',
    dispatchUrl: 'https://svc.run.app/internal/dispatch',
    token,
    deadlineMs: 30 * 60 * 1000,
    now: () => new Date(1_700_000_000 * 1000),
  });
}

describe('CloudTasks', () => {
  it('builds a POST task carrying the envelope and a Bearer token', async () => {
    const f = new FakeSubmitter();
    const ct = newCT(f, 'sekret');
    const e = newEnvelope(Kind.CI, 'webhook:/github', Buffer.from('{"action":"completed"}'), new Date(0));
    await ct.enqueue(e);

    const req = f.request!;
    expect(req.parent).toBe('projects/p/locations/l/queues/q');
    const hr = req.task!.httpRequest!;
    expect(hr.httpMethod).toBe('POST');
    expect(hr.url).toBe('https://svc.run.app/internal/dispatch');
    expect(hr.headers!.Authorization).toBe('Bearer sekret');
    expect(hr.headers!['Content-Type']).toBe('application/json');
    // The body is the exact wire codec output and decodes back to the envelope.
    expect((hr.body as Buffer).equals(encode(e))).toBe(true);
    expect(decode(hr.body as Buffer).kind).toBe(Kind.CI);
    // No dedup name / schedule requested.
    expect(req.task!.name == null).toBe(true);
    expect(req.task!.scheduleTime == null).toBe(true);
    // The dispatch deadline is set explicitly (so a long workflow is not cancelled at the
    // HTTP-target default of 10m and retried, duplicating side effects).
    expect(req.task!.dispatchDeadline?.seconds).toBe(30 * 60);
  });

  it('omits the dispatch deadline when unset (zero)', async () => {
    // With no deadline configured the task omits dispatchDeadline so the queue default applies —
    // production always supplies a config-validated value.
    const f = new FakeSubmitter();
    const ct = new CloudTasks({
      client: f,
      queuePath: 'projects/p/locations/l/queues/q',
      dispatchUrl: 'https://svc.run.app/internal/dispatch',
      token: '',
    });
    await ct.enqueue(env(Kind.CI, ''));
    expect(f.request!.task!.dispatchDeadline == null).toBe(true);
  });

  it('honors the dedup name and schedule delay', async () => {
    const f = new FakeSubmitter();
    const ct = newCT(f, '');
    await ct.enqueue(env(Kind.Coverage, '{}'), { name: 'pr-42', delayMs: 30_000 });

    expect(f.request!.task!.name).toBe('projects/p/locations/l/queues/q/tasks/pr-42');
    expect(f.request!.task!.scheduleTime?.seconds).toBe(1_700_000_030);
    // With no token configured, no Authorization header is attached.
    expect(f.request!.task!.httpRequest!.headers!.Authorization).toBeUndefined();
  });

  it('rejects an oversize envelope up front', async () => {
    // An envelope whose encoded body exceeds the Cloud Tasks task-size limit is refused before
    // create rather than failing opaquely at create time (spec §9).
    const f = new FakeSubmitter();
    const ct = newCT(f, '');
    const big = newEnvelope(Kind.Lint, 's', Buffer.alloc(MAX_TASK_BYTES + 1), new Date(0));
    await expect(ct.enqueue(big)).rejects.toThrow(/task limit/);
    expect(f.request).toBeNull(); // never reached createTask
  });

  it('surfaces a submit error to the caller', async () => {
    // A create failure surfaces (which becomes a 500 -> the webhook source retries, and the
    // queue retries an /internal/dispatch failure).
    const f = new FakeSubmitter(new Error('unavailable'));
    const ct = newCT(f, '');
    await expect(ct.enqueue(env(Kind.CI, ''))).rejects.toThrow(/create task/);
  });

  it('closes the underlying client', async () => {
    const f = new FakeSubmitter();
    await newCT(f, '').close();
    expect(f.closed).toBe(true);
  });
});

describe('CloudTasks trace propagation', () => {
  afterEach(() => {
    trace.disable();
    propagation.disable();
    context.disable();
  });

  it('injects a W3C traceparent header when tracing is enabled', async () => {
    const shutdown = install(new InMemorySpanExporter(), {
      exporter: TracesExporter.Console,
      serviceName: 'automation-agent',
    });
    try {
      const f = new FakeSubmitter();
      const ct = newCT(f, 'sekret');
      // Enqueue under an active span (the ingress request), so the task continues that trace.
      const span = trace.getTracer('t').startSpan('ingress');
      await context.with(trace.setSpan(context.active(), span), () => ct.enqueue(env(Kind.CI, '{}')));
      span.end();
      expect(f.request!.task!.httpRequest!.headers!.traceparent).toBeTruthy();
    } finally {
      await shutdown();
    }
  });

  it('adds no traceparent when tracing is disabled', async () => {
    // With tracing off (no provider registered), inject is a no-op — no traceparent leaks onto the
    // task.
    const f = new FakeSubmitter();
    await newCT(f, 'sekret').enqueue(env(Kind.CI, '{}'));
    expect(f.request!.task!.httpRequest!.headers!.traceparent).toBeUndefined();
  });
});
