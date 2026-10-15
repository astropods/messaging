# Summary

An observe channel left off `allowed_channel_ids` made the bot answer every top-level message and thread reply there with the not-enabled notice. Those messages never addressed the bot, so the bot interrupted a channel it should only watch.

The allowlist stays strict: an observe channel still has to be on `allowed_channel_ids` to reach the agent. A message in an observe channel off the list is now dropped silently, unless it `@`-mentions the bot.

# Design

| Message in an observe channel off the allowlist | Before | After |
|---|---|---|
| Top-level message, no mention | Notice posted, dropped | Dropped, no notice |
| Thread reply | Notice posted, dropped | Dropped, no notice |
| `@`-mention | Notice posted, dropped | Unchanged |

Thread replies are silent too because the sidecar receives every thread reply in a channel, whether or not the thread involves the bot. Only an `@`-mention addresses the bot, so only a mention gets the notice. The check is whether the channel is an observe channel, not whether this one message was observed. The drop still counts under `messaging_messages_dropped_total{reason="allowlist"}`, so a misconfigured observe channel shows up in metrics rather than in the channel.

# Migration

None. To observe a channel while `allowed_channel_ids` is set, list it in both fields.

The public Astropods docs describe `observe_channel_ids` and `allowed_channel_ids` separately and do not say how they combine. Once this ships, document that an observe channel must also be on the allowlist when one is set (`docs-public/fern/docs/pages/deploy-agent.mdx` in `astropods/astro`).
