// Tests for the ingest envelope and Kind.
import { describe, expect, it } from 'vitest';
import { Kind, kindValid, newEnvelope } from './envelope';

describe('ingest', () => {
  it('recognizes valid kinds', () => {
    for (const k of [Kind.CronDaily, Kind.Lint, Kind.Coverage, Kind.CI]) {
      expect(kindValid(k)).toBe(true);
    }
    expect(kindValid('nope')).toBe(false);
  });

  it('constructs an envelope', () => {
    const at = new Date(1718870400 * 1000);
    const e = newEnvelope(Kind.Lint, 'webhook:/lint', Buffer.from('{"x":1}'), at);
    expect(e.kind).toBe(Kind.Lint);
    expect(e.source).toBe('webhook:/lint');
    expect(e.payload.toString()).toBe('{"x":1}');
    expect(e.receivedAt).toBe(at);
  });
});
