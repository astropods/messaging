# Summary

An observe channel left off `allowed_channel_ids` made the bot answer every top-level message there with the not-enabled notice. Those messages never addressed the bot, so the bot interrupted a channel it should only watch.

The allowlist stays strict: an observe channel still has to be on `allowed_channel_ids` to reach the agent. An observed message from a channel off the list is now dropped silently.

# Design

| Message in an observe channel off the allowlist | Before | After |
|---|---|---|
| Top-level message, no mention | Notice posted, dropped | Dropped, no notice |
| Thread reply | Notice posted, dropped | Unchanged |
| `@`-mention | Notice posted, dropped | Unchanged |

Only `handleMessage`'s observe path changes. The drop still counts under `messaging_messages_dropped_total{reason="allowlist"}`, so a misconfigured observe channel shows up in metrics rather than in the channel.

# Migration

None. To observe a channel while `allowed_channel_ids` is set, list it in both fields.

The public Astropods docs describe `observe_channel_ids` and `allowed_channel_ids` separately and do not say how they combine. Once this ships, document that an observe channel must also be on the allowlist when one is set (`docs-public/fern/docs/pages/deploy-agent.mdx` in `astropods/astro`).
