# Summary

An agent could not tell who sent a Slack message by name. The inbound `Message.user` carried only the Slack user id, and `user.username` stayed empty on ordinary messages, app mentions, thread replies, button clicks, and assistant thread starts. Agents that wanted a name had nothing to read, and `platform_data` never carried one.

Part of astropods/astro#3126.

# Design

`dispatch` now sets `Message.user.username` from the existing `slackDirectory.userName` resolver, the one that already names thread-history authors. It reads Slack's `users.info` and returns the display name, then the real name, then the handle. Both hits and misses are cached per pod.

| Case | Behavior |
|---|---|
| Message reaches the agent | `user.username` is the sender's Slack name |
| Linked Slack user | Looked up by `PlatformContext.user_id`, the raw `U…` id. `user.id` still carries the Astro user ID |
| Authz denies or fails | No lookup, since nothing reaches the agent |
| Event already carries a username (slash command, reaction) | Kept as is. These carry Slack's handle, not the display name |
| App lacks `users:read` | `user.username` stays empty. The miss is cached, so Slack is asked once per user per pod |

The lookup runs after authz rather than beside the channel-name lookup, so a denied sender costs no Slack call. All six Slack ingress paths go through `dispatch`, so a new ingress point gets the name too.

The first message from a user not yet in the cache waits on one `users.info` call. Every later message from that user is served from the cache, which holds up to 2,048 users and empties when it fills. A name changed in Slack shows up after the pod restarts, the same as channel names.

# Migration

None. `user.username` is an existing field. Agents that do not read it see no change.
