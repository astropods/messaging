# Summary

Slack ingress gains structured delivery metadata (`[slack_meta]`, optional observer/auto-link preambles, thread summaries) so agents can classify traffic and link back to archives without guessing URLs.

# Design

- **Config** — `SLACK_CONFIG` accepts `observer_channels`, auto-link filters, `channel_messages`, prepend flags, and thread transcript caps; mapped into `adapter.Config` for the Slack adapter.
- **Ingress** — Top-level channel posts bypass the app_mention-only path when the channel is an observer channel, matches auto-link rules, or `channel_messages` is enabled; dedup prevents double delivery with mentions.
- **Content** — DMs, thread replies, mentions, reactions, buttons, and assistant-thread-started events prepend a JSON `[slack_meta]` line (plus `[slack_thread_url]` / `[slack_thread_summary]` where configured). `auth.test` seeds bot user id and workspace URL for permalink fallback.
- **Assistant API** — `invalid_thread_ts` from `assistant.threads.setStatus` is treated as a no-op.

# Migration

No action required unless you want observer or auto-link behavior; set the new `SLACK_CONFIG` fields (or deploy UI `observe_channel_ids` after the follow-up alias PR).
