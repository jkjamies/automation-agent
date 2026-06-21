/**
 * Slack incoming-webhook notifier.
 *
 * The minimal accepted payload is `{ "text": "..." }`.
 */
import type { Message, Notifier } from './notify';
import { postJson } from './notify';

/** Posts to a Slack incoming webhook. */
export class SlackNotifier implements Notifier {
  constructor(private readonly url: string) {}

  /** Post the message as Slack mrkdwn. */
  async notify(m: Message): Promise<void> {
    await postJson(this.url, { text: slackText(m) });
  }
}

/** Render a Message as Slack mrkdwn. */
export function slackText(m: Message): string {
  const parts: string[] = [];
  if (m.title) {
    parts.push(`*${m.title}*`);
  }
  if (m.text) {
    parts.push(m.text);
  }
  if (m.link) {
    parts.push(`<${m.link}>`);
  }
  return parts.join('\n');
}
