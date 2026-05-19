import type { Message, PlatformContext } from './messaging-client';

export interface SlackMetaPayload {
  channel_id: string;
  message_ts: string;
  thread_ts?: string;
  permalink?: string;
}

const SLACK_META_PREFIX = '[slack_meta] ';

export function formatSlackMetaLine(
  channelId: string,
  messageTs: string,
  threadRootId: string | undefined,
  permalink: string | undefined
): string {
  const payload: SlackMetaPayload = {
    channel_id: channelId,
    message_ts: messageTs,
  };
  if (threadRootId && threadRootId !== messageTs) {
    payload.thread_ts = threadRootId;
  }
  if (permalink) {
    payload.permalink = permalink;
  }
  return SLACK_META_PREFIX + JSON.stringify(payload);
}

export function resolveSlackPermalink(
  teamUrl: string | undefined,
  channelId: string,
  messageTs: string
): string | undefined {
  if (!channelId || !messageTs || !teamUrl) {
    return undefined;
  }
  const base = teamUrl.replace(/\/$/, '');
  const ts = messageTs.replace(/\./g, '');
  return `${base}/archives/${channelId}/p${ts}`;
}

function permalinkFromContext(ctx: PlatformContext): string | undefined {
  const fromData = ctx.platformData?.permalink ?? ctx.platformData?.slack_permalink;
  if (fromData) {
    return fromData;
  }
  const teamUrl = ctx.platformData?.team_url ?? ctx.platformData?.workspace_url;
  return resolveSlackPermalink(teamUrl, ctx.channelId, ctx.messageId);
}

export function prependSlackMeta(content: string, metaLine: string): string {
  if (!metaLine) {
    return content;
  }
  if (!content) {
    return metaLine;
  }
  return `${metaLine}\n${content}`;
}

export function hasSlackMeta(content: string): boolean {
  return content.startsWith(SLACK_META_PREFIX);
}

/** Prepend `[slack_meta]` from PlatformContext. No-op for non-Slack or if already present. */
export function enrichSlackInboundMessage(message: Message): Message {
  if (message.platform !== 'slack' || !message.platformContext) {
    return message;
  }
  if (hasSlackMeta(message.content)) {
    return message;
  }
  const ctx = message.platformContext;
  const metaLine = formatSlackMetaLine(
    ctx.channelId,
    ctx.messageId,
    ctx.threadRootId,
    permalinkFromContext(ctx)
  );
  return {
    ...message,
    content: prependSlackMeta(message.content, metaLine),
  };
}

export function enrichIncomingAgentResponse<T extends { incomingMessage?: Message }>(
  response: T
): T {
  if (!response.incomingMessage) {
    return response;
  }
  return {
    ...response,
    incomingMessage: enrichSlackInboundMessage(response.incomingMessage),
  };
}
