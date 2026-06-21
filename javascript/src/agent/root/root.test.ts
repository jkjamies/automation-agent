// Tests for the root dispatcher.
import { BaseAgent, type Event } from '@google/adk';
import { describe, expect, it } from 'vitest';

import { type Envelope, Kind, newEnvelope } from '../../ingest/envelope';
import { textEvent } from '../setup/events';
import { buildRootDispatcher } from './agentsSetup';
import { Dispatcher } from './root';

function env(kind: Kind): Envelope {
  return newEnvelope(kind, 'test', Buffer.alloc(0), new Date(1000));
}

// A code agent that emits one event, used to build a real runner without an LLM.
class TrivialAgent extends BaseAgent {
  protected override async *runAsyncImpl(): AsyncGenerator<Event, void> {
    yield textEvent(this.name, 'done');
  }
  protected override async *runLiveImpl(): AsyncGenerator<Event, void> {
    // not used
  }
}

describe('Dispatcher', () => {
  it('routes by kind', async () => {
    const d = new Dispatcher();
    const got: Kind[] = [];
    d.register(Kind.CronDaily, async (e) => {
      got.push(e.kind);
    });
    expect(d.handles(Kind.CronDaily)).toBe(true);
    await d.dispatch(env(Kind.CronDaily));
    expect(got).toEqual([Kind.CronDaily]);
  });

  it('no-ops an unhandled kind', async () => {
    const d = new Dispatcher();
    expect(d.handles(Kind.Lint)).toBe(false);
    await expect(d.dispatch(env(Kind.Lint))).resolves.toBeUndefined();
  });

  it('propagates a handler error', async () => {
    const d = new Dispatcher();
    d.register(Kind.CI, async () => {
      throw new Error('handler failed');
    });
    await expect(d.dispatch(env(Kind.CI))).rejects.toThrow('handler failed');
  });
});

describe('buildRootDispatcher', () => {
  it('registers and drives the daily and weekly summary handlers through real runners', async () => {
    const d = buildRootDispatcher({
      summaryDaily: new TrivialAgent({ name: 'daily' }),
      summaryWeekly: new TrivialAgent({ name: 'weekly' }),
    });
    expect(d.handles(Kind.CronDaily)).toBe(true);
    expect(d.handles(Kind.CronWeekly)).toBe(true);
    await expect(d.dispatch(env(Kind.CronDaily))).resolves.toBeUndefined();
    await expect(d.dispatch(env(Kind.CronWeekly))).resolves.toBeUndefined();
  });

  it('registers only the daily cron when no weekly agent is given', () => {
    const d = buildRootDispatcher({ summaryDaily: new TrivialAgent({ name: 'daily' }) });
    expect(d.handles(Kind.CronDaily)).toBe(true);
    expect(d.handles(Kind.CronWeekly)).toBe(false);
  });

  it('registers the fix handlers', async () => {
    const called: Partial<Record<Kind, boolean>> = {};
    const mark = async (e: Envelope) => {
      called[e.kind] = true;
    };
    const d = buildRootDispatcher({ lintKickoff: mark, coverageKickoff: mark, ciResume: mark });
    for (const k of [Kind.Lint, Kind.Coverage, Kind.CI]) {
      expect(d.handles(k)).toBe(true);
      await d.dispatch(env(k));
      expect(called[k]).toBe(true);
    }
  });

  it('registers no cron handlers without summary agents', () => {
    const d = buildRootDispatcher({ summaryDaily: null, summaryWeekly: null });
    expect(d.handles(Kind.CronDaily)).toBe(false);
    expect(d.handles(Kind.CronWeekly)).toBe(false);
  });
});
