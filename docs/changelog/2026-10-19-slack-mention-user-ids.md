# Summary

An agent that collects what people ask of one person missed most asks. A thread reply reaches the agent from every channel the bot is in, but a top-level message reaches it only from a channel listed in `observe_channel_ids`. The sidecar logged `Ignoring top-level message in channel …` and dropped the rest.

Example: Rodric posts `@Sohum can you look at the deploy form?` at the top of a channel Daily Plate is in. With `mention_user_ids: ["U_SOHUM"]`, Daily Plate gets the message and puts it on Sohum's plate. A top-level message that names nobody on the list is still dropped.

# Design

`mention_user_ids` in `SLACK_CONFIG` lists user IDs. The adapter builds `<@U…>` tokens from them at init. A top-level channel message that contains one of the tokens is handled as observed: it is forwarded as `EVENT_KIND_OBSERVED`, deduplicated, and threaded under itself, the same as a message in an observe channel.

Per-user authz is skipped for it, as for an observe channel, because the mentioned person did not address the bot. `dispatch` now treats any `EVENT_KIND_OBSERVED` message as observed, not only one from an observe channel. Off the channel allowlist it is dropped without the not-enabled notice. A message that also mentions the bot still goes through `app_mention` only.

# Migration

None. The setting is empty by default, and nothing changes until it is set. The Astropods deploy form needs a matching field before it can be set there (`apps/astro-server/internal/deployment/template.go` in `astropods/astro`).
