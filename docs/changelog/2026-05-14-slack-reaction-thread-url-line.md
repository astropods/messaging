# Summary

Emoji-reaction forwards to agents now include a dedicated plaintext `[slack_thread_url]` line (plus correct `thread_ts` in `[slack_meta]` when the reacted message is a thread reply) so GitHub filing flows reliably copy a verified Slack URL into issue bodies.

# Design

- **Single resolver** — `resolveSlackPermalink` centralizes `chat.getPermalink` plus workspace archive fallback; `formatSlackMetaLine` accepts an optional cached permalink to avoid duplicate HTTP on reactions.
- **Thread replies** — `fetchReactionMessage` reads `thread_ts` from the Slack message so permalinks target the **thread root** (same view users expect for “this thread”).
- **Plaintext duplicate** — Immediately after `[slack_meta]`, reactions prepend `[slack_thread_url] <url>` when any URL was resolved so models are not dependent on JSON-only `permalink`.

# Migration

None. Downstream agents should treat `[slack_thread_url]` as optional; prefer it or `permalink` inside `[slack_meta]` verbatim for GitHub **Source**.
