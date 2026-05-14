# Summary

Slack-delivered agent text now opens with a structured `[slack_meta]` line (channel, timestamps, optional permalink) for **app mentions**, **channel thread replies**, **DMs**, and **auto-link** top-level forwards — not only observer / channel_messages paths.

# Design

- **Single helper** — `formatSlackMetaLine` centralizes JSON + `chat.getPermalink` so Issueator-style agents always see a verbatim archive URL when Slack resolves it.
- **Order** — For observer channels with `ObserverPrependThread`, `[slack_thread_summary]` is still applied to the raw user text first, then `[slack_meta]` is prepended so the model reads location → transcript → message.
- **auto_link** — Top-level auto-link forwards now include the same `[slack_meta]` line as other bypass paths.

# Migration

None. Downstream agents should treat `[slack_meta]` as optional on any Slack ingress; fields `permalink` / `thread_ts` remain optional in JSON.
