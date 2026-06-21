// Tests for the Slack/Teams notifiers — stubs global fetch to capture posted bodies.
import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  type Message,
  newNotifier,
  SlackNotifier,
  TeamsNotifier,
  teamsCard,
} from './notify';

const URL = 'https://hook.example/webhook';

function stubFetch(status: number, text = ''): ReturnType<typeof vi.fn> {
  const fn = vi.fn(async () => new Response(text, { status }));
  vi.stubGlobal('fetch', fn);
  return fn;
}

function lastBody(fn: ReturnType<typeof vi.fn>): any {
  const init = fn.mock.calls.at(-1)![1] as RequestInit;
  return JSON.parse(init.body as string);
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('notify', () => {
  it('posts a Slack mrkdwn body', async () => {
    const fn = stubFetch(200);
    const m: Message = { title: 'Digest', text: '3 commits', link: 'https://x/pr/1' };
    await new SlackNotifier(URL).notify(m);

    expect(fn).toHaveBeenCalled();
    expect(lastBody(fn).text).toBe('*Digest*\n3 commits\n<https://x/pr/1>');
  });

  it('posts a Teams Adaptive Card', async () => {
    const fn = stubFetch(200);
    await new TeamsNotifier(URL).notify({ title: 'Result', text: 'fixed', link: 'https://x/pr/2' });

    expect(fn).toHaveBeenCalled();
    const payload = lastBody(fn);
    expect(payload.type).toBe('message');
    expect(payload.attachments).toHaveLength(1);
    const att = payload.attachments[0];
    expect(att.contentType).toBe('application/vnd.microsoft.card.adaptive');
    expect(att.content.type).toBe('AdaptiveCard');
    expect(att.content.actions).toBeDefined();
  });

  it('treats a non-2xx response as an error', async () => {
    stubFetch(500, 'boom');
    await expect(new SlackNotifier(URL).notify({ text: 'x' })).rejects.toThrow();
  });

  it('picks the implementation by provider', () => {
    expect(newNotifier('slack', 'https://hook', '')).toBeInstanceOf(SlackNotifier);
    expect(newNotifier('teams', '', 'https://hook')).toBeInstanceOf(TeamsNotifier);
    expect(() => newNotifier('slack', '', '')).toThrow();
    expect(() => newNotifier('discord', 'a', 'b')).toThrow();
  });

  it('omits Teams actions without a link', () => {
    const card = teamsCard({ title: 't', text: 'b' });
    const content = (card.attachments as any)[0].content;
    expect(content.actions).toBeUndefined();
  });
});
