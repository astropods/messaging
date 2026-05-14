# Summary

Adds `observe_channel_ids` to `SLACK_CONFIG`. In those channels the Slack adapter forwards top-level (non-mention) messages to the agent instead of dropping them. Messages that already `@`-mention the bot continue to flow through `app_mention` only — no double-delivery — and Slack retries are deduped.

# Design

- **Single knob** — `ObserveChannelIDs []string`. Empty means no behaviour change.
- **Dedup with `app_mention`** — when any observe channel is configured, init also installs a retry dedup. Top-level messages containing `<@bot_user_id>` are dropped as `app_mention_dedup` so the `message` and `app_mention` events don't both fire.
- **Retry dedup** — a bounded in-memory map (2-minute window, 512 keys) suppresses duplicate `channel:ts` deliveries for observed messages.
- **Threading** — observed forwards set `ThreadId = TimeStamp` so the agent's reply opens a thread under the observed message.
- **Authz bypass** — observe channels are passive watch channels; the user did not address the bot. `dispatch` skips the per-user authz check when the message's channel is in the observe set. Operators implicitly trust everyone posting in those channels.
- **`PlatformContext.trigger`** (proto) — new enum: `TRIGGER_DIRECT` (DMs, mentions, threads, buttons, reactions) or `TRIGGER_OBSERVED` (observe-channel forwards). `TRIGGER_UNSPECIFIED` is treated as direct for backwards compat.
- **`PlatformContext.bot_user_id`** (proto) — the adapter's own bot user ID (resolved once from `auth.test` at init). The adapter strips `<@bot>` from `app_mention` content; this field lets the agent still detect "I was mentioned" in any path, including observed traffic.
- **SDKs** — TypeScript (`@astropods/messaging`) exports `PlatformContextTrigger` plus optional `trigger` / `botUserId` on `PlatformContext`. Python stubs (`astropods_messaging.astro.messaging.v1.message_pb2`) expose `PlatformContext.Trigger` (UNSPECIFIED/DIRECT/OBSERVED) and `bot_user_id`.

# Migration

Set `observe_channel_ids` in `SLACK_CONFIG` for channels where the agent should react to everyone's messages, not just `@`-mentions. SDK consumers can read `platformContext.trigger` / `botUserId` (TS) or `platform_context.trigger` / `bot_user_id` (Python); unset is equivalent to `TRIGGER_DIRECT`.
