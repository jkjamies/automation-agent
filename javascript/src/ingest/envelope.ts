/**
 * The normalized event envelope every ingress source is reduced to.
 *
 * Cron, webhooks, and future hooks (GitHub/Jira/Confluence) are all normalized to
 * an {@link Envelope} before being handed to the root agent. See
 * .agents/standards/architecture-design.md §2.
 */

/** Identifies what triggered an ingest, so the root agent can route it. */
export const Kind = {
  CronDaily: 'cron.daily', // 09:00 daily -> summary digest
  CronWeekly: 'cron.weekly', // 09:00 Monday
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
  source: string; // human-readable origin, e.g. "scheduler", "webhook:/lint"
  receivedAt: Date;
  payload: Buffer;
}

/** Construct an Envelope. */
export function newEnvelope(kind: Kind, source: string, payload: Buffer, at: Date): Envelope {
  return { kind, source, receivedAt: at, payload };
}
