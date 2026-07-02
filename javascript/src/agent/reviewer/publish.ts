/**
 * The CodeRabbit-style publish stage: assembly + REST writes (advisory review, marker summary
 * comment, advisory agent-review check), reconciled against the PR's existing fingerprinted
 * comments. Nothing here gates a merge.
 */

import type { PRFile, ReviewComment, ReviewInput } from '../../githubapi/client';
import { Dimension, type Finding, Severity, fingerprint } from './findings';
import { DiffIndex } from './hunks';
import { fpMarker, reconcile } from './reconcile';
import { maxBacktickRun } from './review';
import { Level, type Scorecard, levelGlyph, levelWord } from './scorecard';
import type { Engine } from './reviewer';

// The advisory check the reviewer publishes (agent-published, human-consumed). Globally unique and
// identical across ports (external contract).
export const CHECK_NAME = 'agent-review';

/** Carries the per-PR identifiers and context the published artifacts need. */
export interface PublishMeta {
  owner: string;
  repo: string;
  number: number;
  headSha: string;
  files: PRFile[]; // for the in-diff index
  tiers: string; // model tiers used, for the Review details section
  standards: string[]; // applied source paths (empty = generic)
}

/**
 * The hidden HTML comment that identifies the reviewer's single summary comment so a re-review
 * updates it rather than posting a new one.
 */
export function summaryMarker(owner: string, repo: string, number: number): string {
  return `<!-- automation-agent:review:${owner}/${repo}#${number} -->`;
}

/**
 * Post the review for a scored PR: inline comments for in-diff actionable findings, a marker-updated
 * summary comment with the scorecard, and the advisory agent-review check. Out-of-diff actionable
 * findings and nitpicks go into the summary (never dropped).
 */
export async function publish(
  engine: Engine,
  card: Scorecard,
  findings: Finding[],
  meta: PublishMeta,
): Promise<void> {
  // At-least-once safety: reconciliation makes the inline comments idempotent, but the check run and
  // summary are create/upsert-only, so a redelivered task for a SHA already published would
  // duplicate the check. If the agent-review check already exists for this head SHA, skip — a
  // genuine re-push carries a new SHA and still reconciles below.
  if (await alreadyPublished(engine, meta)) {
    engine.log.info('review already published for head SHA; skipping re-post', {
      repo: `${meta.owner}/${meta.repo}`,
      sha: meta.headSha,
    });
    return;
  }
  const idx = new DiffIndex(meta.files);
  const { inline, outOfDiff, nitpicks } = classify(findings, idx);
  const actionable = inline.length + outOfDiff.length;

  // Reconcile against the comments already on the PR (GitHub-as-store): keep inline findings that
  // still apply (don't re-post — idempotent), post only new ones, and minimize the comments whose
  // finding is gone.
  const existing = await engine.gh!.listReviewComments(meta.owner, meta.repo, meta.number);
  const rec = reconcile(inline, existing);

  // Post only the new inline findings; an empty review is noise.
  if (rec.toPost.length > 0) {
    const comments: ReviewComment[] = rec.toPost.map((f) => ({
      path: f.file,
      line: f.line,
      side: 'RIGHT',
      body: inlineCommentBody(f),
    }));
    const body = `${levelGlyph(card.overall)} Agent review — see the summary comment for the full scorecard.`;
    const input: ReviewInput = { body, comments };
    await engine.gh!.createReview(meta.owner, meta.repo, meta.number, input);
  }

  // Minimize the comments whose finding no longer applies — best-effort. New inline comments are
  // already posted but the summary and check are not; aborting here on a single minimize failure
  // would leave the PR without its summary/check. So log and continue per node.
  for (const nodeId of rec.toMinimize) {
    try {
      await engine.gh!.minimizeComment(nodeId);
    } catch (err) {
      engine.log.warn('reviewer: minimize outdated comment failed; continuing', {
        repo: `${meta.owner}/${meta.repo}`,
        node: nodeId,
        err: errMsg(err),
      });
    }
  }

  const marker = summaryMarker(meta.owner, meta.repo, meta.number);
  await engine.gh!.upsertMarkerComment(
    meta.owner,
    meta.repo,
    meta.number,
    marker,
    summaryComment(marker, card, actionable, nitpicks, outOfDiff, meta),
  );

  await engine.gh!.createCheckRun(meta.owner, meta.repo, {
    name: CHECK_NAME,
    headSha: meta.headSha,
    conclusion: checkConclusion(card.overall),
    title: `${levelGlyph(card.overall)} Agent review — ${levelWord(card.overall)}`,
    summary: `Overall: ${levelWord(card.overall)} · Actionable comments: ${actionable}`,
  });
}

/**
 * Post the "too large to review" outcome: a marker-updated summary comment framed fail-like (🔴)
 * plus a neutral check. No model call was made.
 */
export async function publishDeny(
  engine: Engine,
  meta: PublishMeta,
  reason: string,
  files: number,
  diffBytes: number,
): Promise<void> {
  if (await alreadyPublished(engine, meta)) {
    engine.log.info('deny already published for head SHA; skipping re-post', {
      repo: `${meta.owner}/${meta.repo}`,
      sha: meta.headSha,
    });
    return;
  }
  const marker = summaryMarker(meta.owner, meta.repo, meta.number);
  const body =
    `${marker}\n## 🔴 Agent review — too large for automated review\n\n` +
    `This PR is too large to review automatically (${files} files / ${diffBytes} bytes ` +
    'after excluding generated files). Please split it into smaller PRs.\n\n' +
    `_${reason}_\n`;
  await engine.gh!.upsertMarkerComment(meta.owner, meta.repo, meta.number, marker, body);
  await engine.gh!.createCheckRun(meta.owner, meta.repo, {
    name: CHECK_NAME,
    headSha: meta.headSha,
    conclusion: 'neutral',
    title: '🔴 Agent review — too large',
    summary: `${files} files / ${diffBytes} bytes after excluding generated files; please split.`,
  });
}

/**
 * Report whether the agent-review check already exists for the head SHA. A lookup error is treated
 * as "not published" so a transient failure never suppresses a real review.
 */
export async function alreadyPublished(engine: Engine, meta: PublishMeta): Promise<boolean> {
  try {
    const res = await engine.gh!.agentCheck(meta.owner, meta.repo, meta.headSha, CHECK_NAME);
    return res.found;
  } catch {
    return false;
  }
}

/**
 * Split confidence-gated findings into inline findings (actionable, on a commentable diff line),
 * out-of-diff actionable findings (listed in the summary, never snapped to a wrong line), and
 * nitpicks (collapsed in the summary).
 */
export function classify(
  findings: Finding[],
  idx: DiffIndex,
): { inline: Finding[]; outOfDiff: Finding[]; nitpicks: Finding[] } {
  const inline: Finding[] = [];
  const outOfDiff: Finding[] = [];
  const nitpicks: Finding[] = [];
  for (const f of findings) {
    if (f.severity === Severity.Nitpick) {
      nitpicks.push(f);
      continue;
    }
    if (f.file !== '' && f.line > 0 && idx.inDiff(f.file, f.line)) {
      inline.push(f);
      continue;
    }
    outOfDiff.push(f);
  }
  return { inline, outOfDiff, nitpicks };
}

/**
 * Render one inline comment: an icon+category prefix, the message, an optional ```suggestion block
 * (a localized fix), and an optional "Prompt for AI agents" block.
 */
export function inlineCommentBody(f: Finding): string {
  const parts: string[] = [];
  // Dimension/severity are normalized to known enums, so only the model-authored message needs
  // sanitizing here.
  parts.push(`**${findingPrefix(f)}** · _${f.dimension}_\n\n${sanitizeText(f.message)}\n`);
  if (f.suggestion !== '') {
    // Suggestion is model-authored; size the outer fence past any backtick run in it so a suggestion
    // containing a ```fence can't close the block early and inject markdown or @mentions.
    let fence = '`'.repeat(maxBacktickRun(f.suggestion) + 1);
    if (fence.length < 3) {
      fence = '```';
    }
    parts.push('\n' + fence + 'suggestion\n');
    parts.push(f.suggestion);
    if (!f.suggestion.endsWith('\n')) {
      parts.push('\n');
    }
    parts.push(fence + '\n');
  }
  if (f.fixPrompt !== '') {
    // FixPrompt is model-authored; render it inside a code fence so any @mentions or HTML are
    // literal (not pinged/injected) and it stays copy-pasteable.
    let fence = '`'.repeat(maxBacktickRun(f.fixPrompt) + 1);
    if (fence.length < 3) {
      fence = '```';
    }
    parts.push('\n<details>\n<summary>🤖 Prompt for AI agents</summary>\n\n');
    parts.push(fence + '\n');
    parts.push(f.fixPrompt);
    if (!f.fixPrompt.endsWith('\n')) {
      parts.push('\n');
    }
    parts.push(fence + '\n\n</details>\n');
  }
  // Hidden fingerprint marker so a later re-review re-identifies this comment and reconciles it.
  parts.push('\n' + fpMarker(fingerprint(f)) + '\n');
  return parts.join('');
}

// Matches an @ immediately followed by a mention character; sanitizeText inserts a zero-width space
// after the @ so GitHub does not render (and notify) it as a mention.
const MENTION_PATTERN = /@([A-Za-z0-9])/g;

/**
 * Neutralize model-authored text for safe embedding in a Markdown comment: escape HTML-significant
 * characters (so a finding can't inject markup such as </details>) and break @mentions with a
 * zero-width space (so the reviewer never pings a real user). Code in ```suggestion blocks and
 * fenced FixPrompt is left untouched by callers.
 */
export function sanitizeText(s: string): string {
  s = s.replaceAll('&', '&amp;');
  s = s.replaceAll('<', '&lt;');
  s = s.replaceAll('>', '&gt;');
  return s.replace(MENTION_PATTERN, '@​$1');
}

/** The icon+category label that leads an inline comment. */
export function findingPrefix(f: Finding): string {
  if (f.dimension === Dimension.Security) {
    return '🔒 Security';
  }
  if (f.severity === Severity.Critical || f.severity === Severity.Major) {
    return '⚠️ Potential issue';
  }
  return '🛠️ Refactor';
}

/**
 * Assemble the marker-updated summary comment: header, scorecard table, and collapsible sections for
 * nitpicks, out-of-diff findings, and review details.
 */
export function summaryComment(
  marker: string,
  card: Scorecard,
  actionable: number,
  nitpicks: Finding[],
  outOfDiff: Finding[],
  meta: PublishMeta,
): string {
  const parts: string[] = [marker, '\n'];
  parts.push(
    `## ${levelGlyph(card.overall)} Agent review — Overall: ${levelWord(card.overall)} · ` +
      `Actionable comments: ${actionable}\n\n`,
  );
  parts.push(scorecardTable(card));
  if (nitpicks.length > 0) {
    parts.push(collapsible(`🧹 Nitpicks (${nitpicks.length})`, findingsList(nitpicks)));
  }
  if (outOfDiff.length > 0) {
    parts.push(collapsible(`🔭 Outside diff range (${outOfDiff.length})`, findingsList(outOfDiff)));
  }
  parts.push(collapsible('Review details', reviewDetails(meta)));
  return parts.join('');
}

/**
 * Render the per-dimension severity histogram. With no findings it states so rather than emitting an
 * empty table.
 */
export function scorecardTable(card: Scorecard): string {
  if (card.dims.length === 0) {
    return '_No findings._\n\n';
  }
  const parts: string[] = [
    '| Dimension | Level | Critical | Major | Medium | Nitpick |\n',
    '|---|---|---|---|---|---|\n',
  ];
  for (const d of card.dims) {
    parts.push(
      `| ${d.dimension} | ${levelGlyph(d.level)} | ${d.critical} | ${d.major} | ` +
        `${d.medium} | ${d.nitpick} |\n`,
    );
  }
  parts.push('\n');
  return parts.join('');
}

/** Render findings as a bulleted file:line list for the summary's collapsible sections. */
export function findingsList(fs: Finding[]): string {
  const parts: string[] = [];
  for (const f of fs) {
    const loc = f.line > 0 ? `${f.file}:${f.line}` : f.file;
    parts.push(`- **${f.severity}** \`${loc}\` _(${f.dimension})_ — ${sanitizeText(f.message)}\n`);
  }
  return parts.join('');
}

/** Render the "Review details" section: head SHA, file count, and the model tiers. */
export function reviewDetails(meta: PublishMeta): string {
  const parts: string[] = [`- Head SHA: \`${meta.headSha}\`\n`, `- Files reviewed: ${meta.files.length}\n`];
  if (meta.tiers !== '') {
    parts.push(`- Model tiers: ${meta.tiers}\n`);
  }
  if (meta.standards.length > 0) {
    parts.push(`- Standards applied: ${meta.standards.join(', ')}\n`);
  } else {
    // Empty also covers standards-off and the discovery/distill fallback, not just a repo with no
    // convention docs — so stay neutral rather than asserting none were found.
    parts.push('- Standards: generic review\n');
  }
  return parts.join('');
}

/** Wrap `body` in a <details> block with the given summary label. */
export function collapsible(summary: string, body: string): string {
  return `\n<details>\n<summary>${summary}</summary>\n\n${body}\n</details>\n`;
}

/**
 * Map the overall grade to the advisory check conclusion: green is success; yellow and red are
 * neutral. It is never failure — the reviewer never gates a merge.
 */
export function checkConclusion(overall: Level): string {
  return overall === Level.Green ? 'success' : 'neutral';
}

/** Extract a message from a thrown value. */
function errMsg(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}
