/**
 * The normalized event envelope every ingress source is reduced to.
 *
 * Cloud Scheduler, webhooks, and future hooks (GitHub/Jira/Confluence) are all
 * normalized to an {@link Envelope} before being handed to the root agent. See
 * .agents/standards/architecture-design.md §2.
 */

/** Identifies what triggered an ingest, so the root agent can route it. */
export const Kind = {
  CronDaily: 'cron.daily', // daily Cloud Scheduler trigger -> summary digest
  Lint: 'lint', // agnostic lint payload -> lint-fixer
  Coverage: 'coverage', // agnostic coverage payload -> coverage-fixer
  CI: 'ci', // GitHub check_run -> resume lint/coverage fixer
} as const;
export type Kind = (typeof Kind)[keyof typeof Kind];

const KNOWN_KINDS: ReadonlySet<string> = new Set(Object.values(Kind));

/** Report whether the given value is a recognized ingest kind. */
export function kindValid(k: string): k is Kind {
  return KNOWN_KINDS.has(k);
}

/**
 * The normalized unit of work.
 *
 * `payload` carries the raw source body (e.g. the lint JSON or check_run event)
 * for the chosen workflow to parse.
 */
export interface Envelope {
  kind: Kind;
  source: string; // human-readable origin, e.g. "internal:/cron/daily", "webhook:/lint"
  receivedAt: Date;
  payload: Buffer;
}

/** Construct an Envelope. */
export function newEnvelope(kind: Kind, source: string, payload: Buffer, at: Date): Envelope {
  return { kind, source, receivedAt: at, payload };
}

/**
 * Thrown by {@link decode} for a permanent (poison) body — a malformed payload, bad base64, or
 * unrecognized kind that a redelivery cannot fix. The Cloud Tasks worker catches *only* this type
 * to ack-and-drop a poison task, so an unexpected error (a genuine bug) surfaces as a retried 500
 * instead of being silently swallowed as a dropped task. (The typed-error analogue of Python's
 * `ValueError` and the Go reference's returned decode error.)
 */
export class DecodeError extends Error {
  constructor(message: string) {
    super(message);
    this.name = 'DecodeError';
  }
}

/**
 * The JSON wire form of an Envelope crossing the task-queue boundary (`tasks` -> POST
 * `/internal/dispatch`). It is an external contract and must stay byte-identical across all
 * four language ports (spec §7). `payload` is an explicit standard-base64 string — never a
 * raw byte array — so an empty/absent payload is the empty string in every port, with no
 * language-specific null/[]/"" divergence.
 */
interface WireEnvelope {
  kind: string;
  source: string;
  received_at: string; // RFC 3339
  payload: string; // standard base64 of the raw bytes ("" when empty)
}

/**
 * Serialize an envelope to its JSON wire form for the Cloud Tasks transport (the in-process
 * transport passes the object directly and never calls this). The bytes follow the cross-port
 * wire contract exactly (spec §7): compact separators (JSON.stringify is already space-free), an
 * RFC 3339 instant with a trailing "Z" and trimmed fractional-second zeros, and a standard-base64
 * payload.
 *
 * Rejects an unknown kind at the enqueue boundary so both transports fail the same way:
 * {@link decode} (and POST /internal/dispatch) already drop an unknown kind as a poison task,
 * so without this the cloudtasks backend would enqueue successfully and silently discard the
 * work later, while inprocess would still hand it to the dispatcher.
 *
 * @throws Error if the envelope's kind is not a recognized ingest kind.
 */
export function encode(e: Envelope): Buffer {
  if (!kindValid(e.kind)) {
    throw new Error(`ingest: unknown kind ${JSON.stringify(e.kind)}`);
  }
  const wire: WireEnvelope = {
    kind: e.kind,
    source: e.source,
    received_at: toRFC3339(e.receivedAt),
    payload: e.payload.toString('base64'),
  };
  return Buffer.from(JSON.stringify(wire), 'utf-8');
}

/**
 * Parse an envelope from its JSON wire form (see {@link encode}) and reject an unknown kind.
 *
 * A malformed body, bad base64, or unrecognized kind is a permanent (poison) error: the
 * caller should ack the delivery rather than retry it — a redelivery cannot fix a poison
 * payload. `source` and `received_at` are informational (only `kind` and `payload` drive
 * dispatch), so an absent (or JSON `null`) value defaults to the zero value — but a present
 * value of the wrong type is a malformed body, not a silent default: the wire schema is typed,
 * so a non-string `source` or a non-string/unparseable `received_at` is a type error, i.e.
 * poison.
 *
 * @throws Error if the body is not valid JSON, is not a JSON object, the kind is unknown, the
 *   payload is not valid standard base64 or not a string, or `source`/`received_at` is present
 *   with the wrong type (or `received_at` is not a parseable RFC 3339 string).
 */
export function decode(b: Buffer | string): Envelope {
  let wire: unknown;
  try {
    wire = JSON.parse(typeof b === 'string' ? b : b.toString('utf-8'));
  } catch (err) {
    throw new DecodeError(`ingest: decode envelope: ${(err as Error).message}`);
  }
  if (typeof wire !== 'object' || wire === null || Array.isArray(wire)) {
    throw new DecodeError('ingest: decode envelope: want a JSON object');
  }
  const w = wire as Record<string, unknown>;

  const kind = w.kind;
  if (typeof kind !== 'string' || !kindValid(kind)) {
    throw new DecodeError(`ingest: unknown kind ${JSON.stringify(kind)}`);
  }

  // The wire payload is always a base64 string (the typed wire schema). A non-string is a
  // malformed body, not a server error, so it joins the poison path; an absent key defaults
  // to "" (empty payload). Decode strictly so trailing junk is rejected, not silently dropped.
  const payloadRaw = w.payload ?? '';
  if (typeof payloadRaw !== 'string') {
    throw new DecodeError(`ingest: decode payload: want a base64 string, got ${typeof payloadRaw}`);
  }
  const payload = strictBase64Decode(payloadRaw);

  const source = wireString(w, 'source');
  const receivedAt = wireReceivedAt(w);
  return newEnvelope(kind, source, payload, receivedAt);
}

/**
 * Format a Date as RFC 3339 with nanosecond precision: an instant with a trailing "Z" whose
 * fractional second has trailing zeros trimmed (a whole second has no fractional part at all).
 * `Date.toISOString` always emits exactly three fractional digits, so ".000" -> "" and e.g.
 * ".500" -> ".5", producing the exact bytes the cross-port wire contract requires (spec §7).
 */
function toRFC3339(d: Date): string {
  return d.toISOString().replace(/\.(\d*?)0+Z$/, (_m, frac: string) => (frac ? `.${frac}Z` : 'Z'));
}

/**
 * Decode a standard-base64 string strictly. Node's `Buffer.from(s, 'base64')` is lenient — it
 * ignores characters outside the alphabet and tolerates missing padding, silently dropping
 * trailing junk — so re-encode and compare to reject anything that is not canonical standard
 * base64 (the wire contract is canonical standard base64).
 *
 * @throws Error if `s` is not canonical standard base64.
 */
function strictBase64Decode(s: string): Buffer {
  const buf = Buffer.from(s, 'base64');
  if (buf.toString('base64') !== s) {
    throw new DecodeError(`ingest: decode payload: ${JSON.stringify(s)} is not valid standard base64`);
  }
  return buf;
}

/**
 * Return `w[key]` as a string from the typed wire schema: an absent key or JSON `null` yields
 * the zero value "", while a present non-string is a malformed body (poison), not a silent
 * default.
 */
function wireString(w: Record<string, unknown>, key: string): string {
  const value = w[key];
  if (value === undefined || value === null) {
    return '';
  }
  if (typeof value !== 'string') {
    throw new DecodeError(`ingest: decode ${key}: want a string, got ${typeof value}`);
  }
  return value;
}

/**
 * A strict RFC 3339 date-time: `YYYY-MM-DDThh:mm:ss`, an optional fractional second, and a
 * `Z` or numeric `±hh:mm` offset (the `T`/`Z` may be lowercase per RFC 3339 §5.6). This gates
 * {@link wireReceivedAt} because `Date.parse` accepts far more than RFC 3339 — date-only
 * (`1970-01-01`), RFC 2822 (`Thu, 01 Jan 1970 ...`), and space-separated forms — which would let
 * a malformed `received_at` slip through here instead of being rejected as poison, breaking the
 * cross-port wire contract (spec §7).
 */
const RFC3339_RE = /^\d{4}-\d{2}-\d{2}[Tt]\d{2}:\d{2}:\d{2}(\.\d+)?([Zz]|[+-]\d{2}:\d{2})$/;

/**
 * Parse `received_at`: an absent key or JSON `null` is the epoch zero value (the zero-value
 * default, never poison); a present non-string, or a present but unparseable RFC 3339 string
 * (including ""), is poison.
 *
 * Validates against {@link RFC3339_RE} before `Date.parse` so only genuine RFC 3339 strings are
 * accepted — not `Date.parse`'s grab-bag of date-only / RFC 2822 / locale formats. The
 * `Date.parse` NaN check stays as the calendar-range backstop (e.g. `2026-13-45T..` matches the
 * shape but is not a real date).
 */
function wireReceivedAt(w: Record<string, unknown>): Date {
  const value = w.received_at;
  if (value === undefined || value === null) {
    return new Date(0);
  }
  if (typeof value !== 'string') {
    throw new DecodeError(`ingest: decode received_at: want an RFC 3339 string, got ${typeof value}`);
  }
  const ms = RFC3339_RE.test(value) ? Date.parse(value) : NaN;
  if (Number.isNaN(ms)) {
    throw new DecodeError(`ingest: decode received_at: ${JSON.stringify(value)} is not a valid RFC 3339 timestamp`);
  }
  return new Date(ms);
}
