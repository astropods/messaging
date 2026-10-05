# Summary

Watching a Slack channel took two separate acts, and missing either was silent:
list the channel in `observe_channel_ids`, and invite the bot to it. Slack
delivers channel events only to member apps, so a listed channel the bot was
never invited to produces nothing — no events, no error, no log. Every Slack
agent on the platform carried that papercut, and each worked around it in its
own way or not at all.

`join_observed_channels` makes the adapter add itself to every public channel in
`observe_channel_ids` at startup. One list, one token, one place.

# Design

**It belongs in the adapter, not in an agent.** The adapter already holds the
canonical channel list, an authenticated client, and makes a Web API call at
init to resolve the bot user. An agent implementing this needs a second copy of
the channel list and a second copy of the bot token, and both copies drift: an
agent that did exactly that shipped with a list that disagreed by one duplicate
and one missing channel, and with a token that had been rotated out from under
it while the adapter's stayed valid.

**Off by default.** Joining a channel posts a visible membership change in
someone's workspace, so it is opt-in rather than inferred from
`observe_channel_ids` being non-empty. `join_observed_channels: true` in
`SLACK_CONFIG` turns it on.

**Failures never block startup.** `conversations.join` is idempotent and
public-only. A private channel answers `method_not_supported_for_channel_type`
and still needs one manual invite, and a workspace where some channels cannot be
joined must still serve the rest, so each failure is logged per channel and the
loop continues.

# Migration

None. Existing deployments are unaffected until they set
`join_observed_channels`. Turning it on requires the `channels:join` scope,
which means reinstalling the Slack app — and reinstalling rotates the bot
token, so any other copy of that token has to be updated at the same time.
