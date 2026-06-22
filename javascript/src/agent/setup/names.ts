/**
 * safeName maps a repo or file path to an ADK-agent-name-safe string (every
 * non-alphanumeric character becomes `_`). Shared by the workflow agents that derive a
 * sub-agent name from a path (fixflow analyze, summary fetchers). Mirrors the Go
 * reference's setup.SafeName.
 */
export function safeName(s: string): string {
  return [...s].map((ch) => (/[a-zA-Z0-9]/.test(ch) ? ch : '_')).join('');
}
