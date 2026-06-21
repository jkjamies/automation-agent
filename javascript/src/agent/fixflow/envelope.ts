/**
 * The kickoff envelope a CI job posts.
 *
 * `repo`/`base` identify where to work (trusted); `report` is arbitrary tool output
 * (lint report, coverage report, …) the triage LLM reasons over. The report is kept as
 * raw JSON text so {@link Kickoff.reportText} can unquote a JSON-string report (wrapping
 * text/XML like lcov or JaCoCo) while passing any other JSON value through verbatim.
 */

function splitRepo(s: string): [string, string, boolean] {
  const i = s.indexOf('/');
  if (i < 0) {
    return ['', '', false];
  }
  const owner = s.slice(0, i);
  const repo = s.slice(i + 1);
  if (owner === '' || repo === '') {
    return ['', '', false];
  }
  return [owner, repo, true];
}

/** The trusted envelope a CI job posts. `report` is the raw JSON text of the report value. */
export class Kickoff {
  constructor(
    readonly repo: string,
    readonly report: string,
    readonly base: string = 'main',
  ) {}

  /**
   * Check the trusted fields; the report is intentionally not parsed.
   *
   * @throws Error if `repo` is missing/malformed or `report` is empty.
   */
  validate(): void {
    if (this.repo.trim() === '') {
      throw new Error('kickoff: repo is required');
    }
    if (!splitRepo(this.repo)[2]) {
      throw new Error(`kickoff: repo ${JSON.stringify(this.repo)} must be owner/name`);
    }
    if (this.report.trim() === '') {
      throw new Error('kickoff: report is required');
    }
  }

  /**
   * Return the report as clean text for the LLM. A JSON-string report (wrapping
   * text/XML) is unquoted; any other JSON value is passed through as-is.
   */
  reportText(): string {
    const s = this.report.trim();
    if (s.startsWith('"')) {
      try {
        const unquoted = JSON.parse(this.report);
        if (typeof unquoted === 'string') {
          return unquoted;
        }
      } catch {
        // fall through
      }
    }
    return this.report;
  }

  owner(): string {
    return splitRepo(this.repo)[0];
  }

  name(): string {
    return splitRepo(this.repo)[1];
  }
}

/** Parse and validate the envelope, applying defaults. */
export function parseKickoff(b: Buffer | string): Kickoff {
  let raw: unknown;
  try {
    raw = JSON.parse(typeof b === 'string' ? b : b.toString('utf-8'));
  } catch (err) {
    throw new Error(`parse kickoff: ${(err as Error).message}`);
  }
  if (typeof raw !== 'object' || raw === null || Array.isArray(raw)) {
    throw new Error('parse kickoff: expected a JSON object');
  }
  const obj = raw as Record<string, unknown>;

  const repo = typeof obj.repo === 'string' ? obj.repo : '';
  const base = typeof obj.base === 'string' && obj.base !== '' ? obj.base : 'main';
  // Preserve the report's raw JSON text so reportText() can unquote string reports.
  const report =
    obj.report !== undefined && obj.report !== null ? JSON.stringify(obj.report) : '';

  const k = new Kickoff(repo, report, base);
  k.validate();
  return k;
}
