/**
 * The summary workflow's code agents and formatting helpers.
 *
 * The fetch agents write per-repo commit data to `commits:<owner/repo>` state keys; the
 * summarizer's instruction provider reads them and appends them to the prompt; the
 * notifier posts the `digest` state key.
 */
import {
  BaseAgent,
  type Event,
  type InvocationContext,
  type ReadonlyContext,
} from '@google/adk';

import type { Commit } from '../../githubapi/client';
import { type Message, type Notifier } from '../../notify/notify';
import { stateString, textEvent } from '../setup/events';
import { safeName } from '../setup/names';

export const STATE_PREFIX = 'commits:'; // one key per repo: commits:<owner/repo>
export const DIGEST_KEY = 'digest'; // summarizer output

/**
 * The slice of `githubapi` the fetchers need (consumer-defined for fakeability).
 * `githubapi.Client` satisfies this interface.
 */
export interface CommitLister {
  listCommitsSince(owner: string, repo: string, since: Date): Promise<Commit[]>;
}

/** The default injectable clock for the summary workflow. */
export function defaultNow(): Date {
  return new Date();
}

/**
 * Fetches the last `windowMs` of commits for one repo and writes a formatted digest to
 * state under `commits:<repo>`.
 */
class FetchAgent extends BaseAgent {
  constructor(
    private readonly repo: string,
    private readonly gh: CommitLister,
    private readonly windowMs: number,
    private readonly now: () => Date,
  ) {
    super({ name: 'fetch_' + safeName(repo), description: `Fetches recent commits for ${repo}` });
  }

  protected override async *runAsyncImpl(): AsyncGenerator<Event, void> {
    const parts = splitRepo(this.repo);
    if (!parts) {
      throw new Error(`invalid repo ${JSON.stringify(this.repo)} (want owner/repo)`);
    }
    const [owner, name] = parts;
    let commits: Commit[];
    try {
      const since = new Date(this.now().getTime() - this.windowMs);
      commits = await this.gh.listCommitsSince(owner, name, since);
    } catch (err) {
      throw new Error(`fetch ${this.repo}: ${(err as Error).message}`);
    }
    const text = formatCommits(this.repo, commits);
    yield textEvent(name, text, { [STATE_PREFIX + this.repo]: text });
  }

  protected override async *runLiveImpl(): AsyncGenerator<Event, void> {
    // not used
  }
}

/** Posts the summarizer's digest to chat. */
class NotifyAgent extends BaseAgent {
  constructor(
    private readonly notifier: Notifier,
    private readonly title: string,
  ) {
    super({ name: 'notify', description: 'Posts the commit digest to Slack or Teams' });
  }

  protected override async *runAsyncImpl(ctx: InvocationContext): AsyncGenerator<Event, void> {
    let digest = stateString(ctx.session.state, DIGEST_KEY).trim();
    if (digest === '') {
      digest = '(no digest was produced)';
    }
    const m: Message = { title: this.title, text: digest };
    try {
      await this.notifier.notify(m);
    } catch (err) {
      throw new Error(`notify: ${(err as Error).message}`);
    }
    yield textEvent('notify', 'Posted digest to chat.');
  }

  protected override async *runLiveImpl(): AsyncGenerator<Event, void> {
    // not used
  }
}

/** Return a code agent that fetches recent commits for `repo` into state. */
export function newFetchAgent(
  repo: string,
  gh: CommitLister,
  windowMs: number,
  now: () => Date,
): BaseAgent {
  return new FetchAgent(repo, gh, windowMs, now);
}

/**
 * Return a code agent that posts the digest to chat under the given title (e.g.
 * "Daily commit digest" / "Weekly commit digest").
 */
export function newNotifyAgent(notifier: Notifier, title: string): BaseAgent {
  return new NotifyAgent(notifier, title);
}

/**
 * The dynamic instruction for the summarizer: reads the per-repo commit data the
 * fetchers wrote to state and appends it to the prompt body. Returning a function means
 * `{key}` state-injection templating is bypassed, so commit text with braces passes
 * through verbatim.
 */
export function summaryInstruction(promptBody: string): (ctx: ReadonlyContext) => string {
  return (ctx: ReadonlyContext) => buildInstruction(promptBody, ctx.state.toRecord());
}

/**
 * Append every `commits:*` string value in `state` (sorted by key) to the prompt body
 * under a `## Commits` heading.
 */
export function buildInstruction(promptBody: string, state: Record<string, unknown>): string {
  const items: Array<[string, string]> = [];
  for (const [k, v] of Object.entries(state)) {
    if (k.startsWith(STATE_PREFIX) && typeof v === 'string') {
      items.push([k, v]);
    }
  }
  items.sort((a, b) => (a[0] < b[0] ? -1 : a[0] > b[0] ? 1 : 0));

  const parts = [promptBody, '\n\n## Commits\n'];
  if (items.length === 0) {
    parts.push('(no commit data)\n');
  }
  for (const [, value] of items) {
    parts.push(value, '\n');
  }
  return parts.join('');
}

/** Format a repo's commits for the digest. */
export function formatCommits(repo: string, commits: Commit[]): string {
  if (commits.length === 0) {
    return `Repository ${repo}: no commits in the window.`;
  }
  const lines = [`Repository ${repo} (${commits.length} commits):\n`];
  for (const c of commits) {
    lines.push(`- ${shortSha(c.sha)} ${firstLine(c.message)} (${c.author})\n`);
  }
  return lines.join('');
}

/** Return the first line of `s`, trimmed. */
export function firstLine(s: string): string {
  const i = s.indexOf('\n');
  return (i >= 0 ? s.slice(0, i) : s).trim();
}

/** Return the 7-character short SHA (or the whole SHA if shorter). */
export function shortSha(sha: string): string {
  return sha.length > 7 ? sha.slice(0, 7) : sha;
}

/** Split `owner/repo` into its parts, or null if malformed. */
export function splitRepo(s: string): [string, string] | null {
  const i = s.indexOf('/');
  if (i < 0) {
    return null;
  }
  const owner = s.slice(0, i);
  const repo = s.slice(i + 1);
  if (owner === '' || repo === '') {
    return null;
  }
  return [owner, repo];
}
