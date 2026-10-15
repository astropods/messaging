# Summary

Setting `allowed_channel_ids` blocked every direct message to the bot. A DM has its own `D…` channel ID, so it never matched the list, and the user got the not-enabled notice. Slash commands typed in a DM were dropped the same way.

The channel list now governs channels only. A DM passes it.

# Design

| `allowed_channel_ids` | `allowed_user_ids` | DM from user U |
|---|---|---|
| empty | empty | allowed |
| set | empty | allowed (was blocked) |
| any | set | allowed only if U is listed, as before |

`isAllowed` is the single check for messages, mentions, and slash commands, so the change covers all three. Channel behavior is unchanged.

A DM is not open to everyone as a result. `dispatch` still runs the per-user authz check on it, so interface access grants decide who can DM the agent. That matches the deprecation note on `allowed_user_ids`, which points operators at grants for per-user access. To stop DMs entirely, turn off the app's Messages tab in Slack.

# Migration

None. Deployments with `allowed_channel_ids` set start accepting DMs from users their interface access allows.

The public Astropods docs describe `allowed_channel_ids` without saying how it treats DMs. Once this ships, document that the channel list does not apply to direct messages (`docs-public/fern/docs/pages/deploy-agent.mdx` in `astropods/astro`).
