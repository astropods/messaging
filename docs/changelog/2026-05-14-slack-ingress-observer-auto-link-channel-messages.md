# Summary

The Slack adapter distinguishes three ways top-level channel traffic reaches the agent: explicit **observer** channels, **auto-link** substring matches (operator-defined, case-insensitive), and optional **channel_messages** mode (all top-level posts in allowlisted channels). Reactions can carry a permalink and an optional bounded thread transcript. Duplicate top-level deliveries are suppressed with structured logs and labeled metrics.

# Design

- **Precedence** — For a single message, bypass path is `observer` → `auto_link` → `channel_messages`. That path drives dedup log `bypass_path`, Prometheus `SlackEvents` labels (`observer_top`, `auto_link_top`, `channel_messages_top`), and the in-band preamble tag.
- **Preambles** — Observer traffic is prefixed with `[slack_observer]` and a `[slack_meta]` JSON line (`channel_id`, `thread_ts`, `message_ts`). Auto-link matches use `[slack_auto_link]`. Channel-messages-only traffic uses `[slack_channel_messages]` plus the same `[slack_meta]` shape so downstream agents can key storage without protocol changes.
- **`channel_messages` + dedup** — When enabled, init calls `auth.test` to cache the bot user id; top-level channel text containing `<@that_id>` is dropped as `app_mention_dedup` so `message` and `app_mention` events do not double-deliver.
- **Reactions** — Configurable emoji set; optional thread prepend and permalink injection for actionable reactions.
- **Configuration** — `adapter.Config` is populated from `SLACK_CONFIG` JSON (`modules/messaging/config`); fields include `observer_channels`, `auto_link_text_substrings`, `auto_link_channel_ids`, `channel_messages`, prepend limits, and reaction/observer thread caps.

# Migration

- Set or omit keys in `SLACK_CONFIG` / deployment JSON as needed. No migration for deployments that leave `channel_messages` unset (false). Slack operators should tighten `allowed_channel_ids` before turning on `channel_messages`.
