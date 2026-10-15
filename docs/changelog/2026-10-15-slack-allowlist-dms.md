# Summary

Setting `allowed_channel_ids` blocks every direct message to the bot. A DM has its own `D…` channel ID, so it never matches the list, and the user gets the not-enabled notice. An operator could not run "these channels, plus DMs", because DM IDs are per user and cannot reasonably be listed.

`allow_direct_messages` in `SLACK_CONFIG` now lets direct messages with the bot through the channel list. It is off by default, so the channel list stays strict and no deployment changes behavior on upgrade.

```json
{ "allowed_channel_ids": ["C0123ABCDEF"], "allow_direct_messages": true }
```

# Design

| `allowed_channel_ids` | `allow_direct_messages` | DM with the bot |
|---|---|---|
| empty | any | allowed |
| set | off (default) | blocked with the notice, as before |
| set | on | allowed |

When `allowed_user_ids` is set, it still applies to DMs either way.

The exemption covers only message events, through the `botDM` argument to `isAllowed`. Slack sends a DM message event only for the bot's own conversations, so a `D…` channel there is always a DM with the bot. A slash command can be typed in any DM, including one between two people that the bot is not in. Slash commands and mentions therefore pass `botDM=false` and stay under the channel list.

An exempted DM still goes through the per-user authz check in `dispatch`. A fresh Slack deployment is seeded with an `anyone` grant (`seedFreshAuthGrants` in astropods/astro), so with the option on, anyone in the workspace can DM the agent unless the deployment's interface access is narrowed.

# Migration

None. The option is off by default, so behavior is unchanged until an operator sets it.

To accept DMs while keeping a channel list, add `"allow_direct_messages": true` to `SLACK_CONFIG`. Then narrow the Slack interface access if the default `anyone` grant is too wide.

`allow_direct_messages` is not yet a field on the deploy form, which lists fixed `SLACK_CONFIG` fields (`template.go` in astropods/astro). Until it is added, set it with `--var 'SLACK_CONFIG=…'` on the CLI. Locally, set it under `dev.interfaces.messaging.slack` in `astropods.yml`. Once this ships, the public docs should describe the option on the deploy page's "Restrict the bot to channels" section.
