// Tests for the ingest envelope, Kind, and the wire codec.
import { describe, expect, it } from 'vitest';
import { decode, encode, Kind, kindValid, newEnvelope } from './envelope';

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

describe('wire codec', () => {
  // The codec round-trips every field, including a payload that is not valid UTF-8 (it travels
  // as base64, so arbitrary bytes survive) and an empty payload.
  it.each([
    ['json', Buffer.from('{"action":"completed"}')],
    ['binary', Buffer.from([0x00, 0xff, 0xfe, 0x10, 0x80])],
    ['empty', Buffer.alloc(0)],
  ])('round-trips a %s payload', (_name, payload) => {
    const at = new Date(1718870400 * 1000);
    const e = newEnvelope(Kind.CI, 'webhook:/github', payload, at);
    const out = decode(encode(e));
    expect(out.kind).toBe(e.kind);
    expect(out.source).toBe(e.source);
    expect(out.receivedAt.getTime()).toBe(e.receivedAt.getTime());
    expect(out.payload.equals(payload)).toBe(true);
  });

  it('emits the byte-identical wire shape', () => {
    // Byte-identical to the Go reference: compact separators (no spaces), a UTC instant spelled
    // with a trailing "Z" and Go-style trimmed fractional seconds, and a standard-base64 payload
    // ("hi" -> "aGk=").
    const b = encode(newEnvelope(Kind.Lint, 'webhook:/lint', Buffer.from('hi'), new Date(0)));
    expect(b.toString('utf-8')).toBe(
      '{"kind":"lint","source":"webhook:/lint","received_at":"1970-01-01T00:00:00Z","payload":"aGk="}',
    );
  });

  it('trims trailing fractional-second zeros like Go RFC3339Nano', () => {
    // A sub-second instant keeps only the significant fractional digits (".500" -> ".5").
    const b = encode(newEnvelope(Kind.CI, 's', Buffer.alloc(0), new Date(500)));
    expect(b.toString('utf-8')).toContain('"received_at":"1970-01-01T00:00:00.5Z"');
  });

  it('rejects an unknown kind at encode', () => {
    const bad = { kind: 'jira', source: 's', receivedAt: new Date(0), payload: Buffer.alloc(0) };
    expect(() => encode(bad as never)).toThrow(/unknown kind/);
  });

  it('rejects malformed bodies as poison', () => {
    expect(() => decode(Buffer.from('not json'))).toThrow();
    // A JSON array/scalar is not an envelope object.
    expect(() => decode('[]')).toThrow(/JSON object/);
    expect(() => decode('42')).toThrow(/JSON object/);
    // Unknown / non-string kind.
    expect(() => decode('{"kind":"jira","source":"x"}')).toThrow(/unknown kind/);
    expect(() => decode('{"kind":123,"payload":""}')).toThrow(/unknown kind/);
    // Undecodable payload.
    expect(() => decode('{"kind":"ci","source":"x","payload":"@@@not-base64"}')).toThrow();
    // Strict base64: a valid alphabet with trailing junk is rejected (lenient decoding would
    // silently drop it), matching Go's base64.StdEncoding.
    expect(() => decode('{"kind":"ci","source":"x","payload":"aGk=junk"}')).toThrow(/base64/);
    // Missing padding is also rejected (standard base64 requires it).
    expect(() => decode('{"kind":"ci","source":"x","payload":"aGk"}')).toThrow(/base64/);
    // A non-string payload is a malformed body, not a server error — still poison.
    expect(() => decode('{"kind":"ci","source":"x","payload":123}')).toThrow();
    // The typed wire schema rejects a non-string source/received_at as a type error (poison).
    expect(() => decode('{"kind":"ci","source":123,"payload":"aGk="}')).toThrow(/source/);
    expect(() => decode('{"kind":"ci","source":"x","received_at":0,"payload":"aGk="}')).toThrow(/received_at/);
    expect(() => decode('{"kind":"ci","source":"x","received_at":false,"payload":"aGk="}')).toThrow(/received_at/);
    // A present-but-unparseable received_at string (including "") is poison.
    expect(() => decode('{"kind":"ci","source":"x","received_at":"not-a-date","payload":"aGk="}')).toThrow();
    expect(() => decode('{"kind":"ci","source":"x","received_at":"","payload":"aGk="}')).toThrow();
  });

  it('defaults absent or null metadata to the zero value', () => {
    // Absent or JSON null source/received_at default to the zero value (Go: the typed struct's
    // zero / its UnmarshalJSON treating null as a no-op), never poison.
    const epoch = new Date(0).getTime();
    let out = decode('{"kind":"ci","payload":"aGk="}');
    expect(out.source).toBe('');
    expect(out.receivedAt.getTime()).toBe(epoch);
    out = decode('{"kind":"ci","source":null,"received_at":null,"payload":"aGk="}');
    expect(out.source).toBe('');
    expect(out.receivedAt.getTime()).toBe(epoch);
  });
});
