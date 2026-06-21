/**
 * Post provider-agnostic messages to a chat destination (Slack or Teams).
 *
 * Both providers sit behind a single {@link Notifier} interface, so the workflow
 * choice is a config flag, not a code change. Deterministic tooling — it must not
 * import agents.
 */

/** A provider-agnostic notification. */
export interface Message {
  title?: string; // short bold heading
  text?: string; // body
  link?: string; // optional URL (e.g. a PR) rendered as an action/link
}

/** Posts messages to a chat destination. */
export interface Notifier {
  /** Post the message, throwing on failure. */
  notify(m: Message): Promise<void>;
}

/**
 * Return a Notifier for the given provider ("slack" or "teams").
 *
 * @throws Error if the required webhook URL is empty or the provider is unknown.
 */
export function newNotifier(provider: string, slackUrl: string, teamsUrl: string): Notifier {
  if (provider === 'slack') {
    if (slackUrl === '') {
      throw new Error('SLACK_WEBHOOK_URL is required for notify provider slack');
    }
    return new SlackNotifier(slackUrl);
  }
  if (provider === 'teams') {
    if (teamsUrl === '') {
      throw new Error('TEAMS_WEBHOOK_URL is required for notify provider teams');
    }
    return new TeamsNotifier(teamsUrl);
  }
  throw new Error(`unknown notify provider ${JSON.stringify(provider)} (want slack|teams)`);
}

/** POST `payload` as JSON, throwing on a non-2xx status. */
export async function postJson(url: string, payload: unknown): Promise<void> {
  const resp = await fetch(url, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(payload),
    signal: AbortSignal.timeout(10_000),
  });
  if (resp.status < 200 || resp.status >= 300) {
    const body = await resp.text().catch(() => '');
    const snippet = body.slice(0, 512);
    throw new Error(`notification rejected: ${resp.status} ${resp.statusText}: ${snippet}`);
  }
}

// SlackNotifier and TeamsNotifier are re-exported here so notify has a single
// import surface.
import { SlackNotifier } from './slack';
import { TeamsNotifier } from './teams';
export { SlackNotifier, slackText } from './slack';
export { TeamsNotifier, teamsCard } from './teams';
