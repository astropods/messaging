## Summary

Adds a `channel_messages` mode to the Slack adapter so the agent receives all top-level messages posted in a channel, not just @mentions.

Previously, any message posted at the top level of a channel was silently dropped unless it @mentioned the bot — by design, to avoid duplicate dispatch with the `app_mention` event. This made it impossible to deploy the agent as a passive listener or general channel assistant.

## Design

**Config flag.** A new `channel_messages` boolean field is added to `SlackAdapterConfig` (parsed from `SLACK_CONFIG` JSON) and propagated to `adapter.Config`. Default is `false`; existing deployments are unaffected.

```json
{"channel_messages": true}
```

**Deduplication.** When a user @mentions the bot, Slack fires both a `message` event and an `app_mention` event. To prevent the agent receiving the same message twice, the adapter fetches its own bot user ID via `auth.test` at initialization and skips any `message` event whose text contains `<@botUserID>` — those are owned by `handleAppMention`. Non-mention messages have no corresponding `app_mention` event so they pass through unconditionally.

**Auto-threading.** When `auto_thread` is enabled (the default), a top-level channel message uses its own timestamp as the thread root, so the agent's response creates a thread under the original message — matching the behaviour of @mention replies.

**Metric label.** Top-level channel messages forwarded in this mode are counted under the `channel_message` label (previously all non-DM non-reply paths fell into `thread_reply`).

Thread replies are unaffected — they were already forwarded unconditionally regardless of @mention, and remain so.

## Migration

Set `channel_messages: true` in the `SLACK_CONFIG` JSON environment variable for any deployment where the agent should respond to all channel messages rather than only @mentions. No changes required for deployments that keep the default mention-only behaviour.
