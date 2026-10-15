# Summary

Setting `allowed_channel_ids` blocked every direct message to the bot. A DM has its own `D…` channel ID, so it never matched the list, and the user got the not-enabled notice.

A direct message with the bot now passes the channel list. Slash commands stay under it.

# Design

| `allowed_channel_ids` | `allowed_user_ids` | DM from user U |
|---|---|---|
| empty | empty | allowed |
| set | empty | allowed (was blocked) |
| any | set | allowed only if U is listed, as before |

`isAllowed` takes a `botDM` argument, and only `handleMessage` sets it. Slack sends the bot a DM message event only for its own conversations, so a `D…` channel there is always a DM with the bot. A slash command can be typed in any DM, including one between two people that the bot is not in, so `handleSlashCommand` passes `false`, as does `handleAppMention`. Channel behavior is unchanged.

Interface access grants do not narrow this in practice. A fresh Slack deployment is seeded with an `anyone` grant (`seedFreshAuthGrants` in astropods/astro), so the per-user authz check in `dispatch` lets everyone in the workspace through. Who can DM the bot is the operator's call in the Slack app itself: the app's Messages tab turns DMs on or off, the same way channel membership decides where the bot listens.

# Migration

Behavior change. A deployment with `allowed_channel_ids` set used to block every DM to the bot. It now accepts DMs from anyone its interface access allows, which by default is the whole workspace.

To keep DMs off, turn off the Messages tab in the Slack app's settings (**App Home** > **Show Tabs** > **Messages Tab**).

The public Astropods docs describe `allowed_channel_ids` without saying how it treats DMs. Once this ships, document that the channel list does not apply to direct messages (`docs-public/fern/docs/pages/deploy-agent.mdx` in `astropods/astro`).
