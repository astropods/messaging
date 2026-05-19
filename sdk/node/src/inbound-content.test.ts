import { describe, it, expect } from 'bun:test';
import {
  enrichSlackInboundMessage,
  formatSlackMetaLine,
  hasSlackMeta,
  resolveSlackPermalink,
} from './inbound-content';
import type { Message } from './messaging-client';

function slackMessage(overrides: Partial<Message> = {}): Message {
  return {
    platform: 'slack',
    conversationId: 'C1-123.0',
    content: 'hello',
    user: { id: 'U1' },
    platformContext: {
      channelId: 'C1',
      messageId: '100.1',
      threadRootId: '100.0',
    },
    ...overrides,
  };
}

describe('formatSlackMetaLine', () => {
  it('includes thread_ts when different from message_ts', () => {
    const line = formatSlackMetaLine('C1', '100.1', '100.0', 'https://x/p');
    expect(line).toContain('"thread_ts":"100.0"');
    expect(line).toContain('"permalink":"https://x/p"');
  });
});

describe('resolveSlackPermalink', () => {
  it('builds archives URL', () => {
    expect(resolveSlackPermalink('https://acme.slack.com/', 'C99', '1234567890.123456')).toBe(
      'https://acme.slack.com/archives/C99/p1234567890123456'
    );
  });
});

describe('enrichSlackInboundMessage', () => {
  it('prepends meta from platformContext', () => {
    const out = enrichSlackInboundMessage(slackMessage());
    expect(hasSlackMeta(out.content)).toBe(true);
    expect(out.content).toContain('hello');
  });

  it('is idempotent', () => {
    const once = enrichSlackInboundMessage(slackMessage());
    expect(enrichSlackInboundMessage(once).content).toBe(once.content);
  });

  it('skips non-slack platforms', () => {
    const msg: Message = {
      platform: 'web',
      conversationId: 'c',
      content: 'hi',
      user: { id: 'u' },
    };
    expect(enrichSlackInboundMessage(msg).content).toBe('hi');
  });
});
