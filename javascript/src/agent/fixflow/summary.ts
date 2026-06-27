/**
 * The status-aware terminal summary for a finished fix run.
 *
 * A run's per-attempt work product lives only on the PR (commits + diff), never in the
 * session, so {@link SummaryInput.changed} (a base...branch comparison) is how the human
 * learns what the agent actually did. {@link buildSummaryText} frames the outcome
 * (success / exhausted / timeout), inlines the original findings, and appends what changed.
 * Pure (no I/O) so it is unit-testable.
 */
import type { Comparison, ChangedFile } from '../../githubapi/client';

/** The way a fix run ended; selects the summary framing. */
export enum TerminalOutcome {
  Success = 'success',
  Exhausted = 'exhausted',
  Timeout = 'timeout',
  Clean = 'clean', // triage found nothing to address — already clean, no PR opened
}

/** Everything a terminal summary needs. */
export interface SummaryInput {
  outcome: TerminalOutcome;
  workflow: string; // spec.name (lint | coverage)
  fullRepo: string;
  prNumber: number;
  attempts: number;
  report: string; // original targeted findings (runParams.report)
  lastOutput: string; // last CI check output (exhausted) — the remaining findings
  timeout: string; // CI_TIMEOUT (timeout outcome)
  checkName: string; // the awaited check (timeout outcome)
  changed: Comparison;
}

/** Bounds how much of a (potentially large) findings blob a summary inlines. */
const MAX_FINDINGS_RUNES = 280;
/** Bounds how many changed file names a summary lists. */
const MAX_FILES = 8;

/** Frame a terminal outcome into a human notification body. */
export function buildSummaryText(input: SummaryInput): string {
  const changed = changedSummary(input.changed);
  const { fullRepo, workflow } = input;
  switch (input.outcome) {
    case TerminalOutcome.Success: {
      const text = `${fullRepo}: the ${workflow} fix passed CI after ${attemptsPhrase(input.attempts)}. ${changed}`;
      return appendFindings(text, 'Targeted', input.report);
    }
    case TerminalOutcome.Exhausted: {
      const text = `${fullRepo}: the ${workflow} fix still fails CI after ${attemptsPhrase(input.attempts)}. Please review. ${changed}`;
      return appendFindings(text, 'Remaining', input.lastOutput);
    }
    case TerminalOutcome.Timeout: {
      const text = `${fullRepo}: the ${workflow} fix saw no CI result after ${input.timeout} waiting for ${input.checkName} (${attemptsPhrase(input.attempts)}). Please review. ${changed}`;
      return appendFindings(text, 'Targeted', input.report);
    }
    case TerminalOutcome.Clean:
      return cleanText(workflow, fullRepo);
    default:
      return `${fullRepo}: the ${workflow} fix reached an unknown terminal state.`;
  }
}

// Light-hearted "nothing to do" lines, rotated deterministically by repo name (a given repo
// always gets the same line — stable and testable — while different repos vary). The rendered
// line is prefixed with the capitalized workflow name (Lint, Coverage, …) so the relation is
// obvious at a glance. Kept byte-for-byte identical across all four ports (parity); repo names
// are ASCII, so the code-point sum is identical in every language.
const cleanMessages = [
  'nothing to see here 👏',
  'squeaky clean, no work for me 🧹',
  "all tidy already — I'll see myself out 🚪",
  'spotless, not a thing to fix 🫧',
  'already shipshape, standing down ✨',
];

/** Render the clean-outcome body: a workflow-prefixed fun line chosen deterministically by repo. */
function cleanText(workflow: string, fullRepo: string): string {
  let sum = 0;
  for (const c of fullRepo) sum += c.charCodeAt(0);
  const msg = cleanMessages[sum % cleanMessages.length];
  const title = workflow ? workflow.charAt(0).toUpperCase() + workflow.slice(1) : workflow;
  return `${title}: ${msg} — ${fullRepo} is already clean, no PR opened.`;
}

function attemptsPhrase(n: number): string {
  return n === 1 ? '1 attempt' : `${n} attempts`;
}

/** Describe the commits + files of a comparison, truncating a long file list. */
function changedSummary(c: Comparison): string {
  if (c.totalCommits === 0 && c.files.length === 0) {
    return 'No changes were recorded on the PR.';
  }
  const commits = c.totalCommits === 1 ? '1 commit' : `${c.totalCommits} commits`;
  return `${commits} changed ${filesPhrase(c.files)}.`;
}

function filesPhrase(files: ChangedFile[]): string {
  if (files.length === 0) {
    return 'no files';
  }
  const names = files.map((f) => f.path);
  let suffix = '';
  let shown = names;
  if (names.length > MAX_FILES) {
    suffix = ` (+${names.length - MAX_FILES} more)`;
    shown = names.slice(0, MAX_FILES);
  }
  return shown.join(', ') + suffix;
}

/**
 * Append a single-line, length-bounded findings snippet to text, or return text unchanged
 * when the blob is empty.
 */
function appendFindings(text: string, label: string, blob: string): string {
  let snippet = blob.split(/\s+/).filter(Boolean).join(' '); // collapse newlines/whitespace
  if (snippet === '') {
    return text;
  }
  const runes = [...snippet];
  if (runes.length > MAX_FINDINGS_RUNES) {
    snippet = runes.slice(0, MAX_FINDINGS_RUNES).join('') + '…';
  }
  return `${text}\n${label}: ${snippet}`;
}
