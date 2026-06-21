/**
 * Microsoft Teams notifier.
 *
 * Posts an Adaptive Card to a Teams incoming webhook. We target the newer
 * Workflows (Power Automate) format rather than the deprecated Office 365
 * connector MessageCard.
 */
import type { Message, Notifier } from './notify';
import { postJson } from './notify';

/** Posts an Adaptive Card to a Microsoft Teams incoming webhook. */
export class TeamsNotifier implements Notifier {
  constructor(private readonly url: string) {}

  /** Post the message as a Workflows Adaptive Card. */
  async notify(m: Message): Promise<void> {
    await postJson(this.url, teamsCard(m));
  }
}

/** Build the Workflows Adaptive Card envelope for a Message. */
export function teamsCard(m: Message): Record<string, unknown> {
  const body: Array<Record<string, unknown>> = [];
  if (m.title) {
    body.push({
      type: 'TextBlock',
      text: m.title,
      weight: 'Bolder',
      size: 'Medium',
      wrap: true,
    });
  }
  if (m.text) {
    body.push({ type: 'TextBlock', text: m.text, wrap: true });
  }

  const content: Record<string, unknown> = {
    $schema: 'http://adaptivecards.io/schemas/adaptive-card.json',
    type: 'AdaptiveCard',
    version: '1.2',
    body,
  };
  if (m.link) {
    content.actions = [{ type: 'Action.OpenUrl', title: 'Open', url: m.link }];
  }

  return {
    type: 'message',
    attachments: [
      {
        contentType: 'application/vnd.microsoft.card.adaptive',
        content,
      },
    ],
  };
}
