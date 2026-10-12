# Summary

An agent could not tell who sent a message by name. On Slack, the inbound `Message.user` carried only the Slack user id: `user.username` stayed empty on ordinary messages, app mentions, thread replies, button clicks, and assistant thread starts, and `platform_data` never carried a name either. On web chat, the web adapter was wired with no name header at all, so `user.username` was always empty there too.

Both now fill `Message.user.username`, an existing field, so agents get the sender's name with no proto change.

Part of astropods/astro#3126.

# Design

## Slack

`dispatch` sets `Message.user.username` from the existing `slackDirectory.userName` resolver, the one that already names thread-history authors. It reads Slack's `users.info` and returns the full name, then the display name, then the handle. The full name comes first because workspaces often set display names to a handle such as `ada.lovelace`, which reads badly in a greeting. Thread-history authors use the same resolver, so they are named the same way. Both hits and misses are cached per pod.

| Case | Behavior |
|---|---|
| Message reaches the agent | `user.username` is the sender's Slack name |
| Linked Slack user | Looked up by `PlatformContext.user_id`, the raw `U…` id. `user.id` still carries the Astro user ID |
| Authz denies or fails | No lookup, since nothing reaches the agent |
| Event already carries a username (slash command, reaction) | Kept as is. These carry Slack's handle, not the display name |
| App lacks `users:read` | `user.username` stays empty. The miss is cached, so Slack is asked once per user per pod |

The lookup runs after authz rather than beside the channel-name lookup, so a denied sender costs no Slack call. All six Slack ingress paths go through `dispatch`, so a new ingress point gets the name too.

The first message from a user not yet in the cache waits on one `users.info` call. Every later message from that user is served from the cache, which holds up to 2,048 users and empties when it fills. A name changed in Slack shows up after the pod restarts, the same as channel names.

## Web chat

The web adapter reads the signed-in user's display name from `X-Astro-User-Name`, beside the `X-Amzn-Oidc-Identity` user ID it already read. astro-server sets both on every proxied request (astropods/astro#3166), and its proxy forwards only an allowlist of client headers, so a client cannot set either.

| Piece | Behavior |
|---|---|
| `web.NewAstroServerSessionManager()` | The session manager for astro-server's proxy. `cmd/server` uses it in place of an inline `NewHeaderSessionManager` call, so the wiring is under test |
| `HeaderUserName` | Path-escaped by astro-server, since a header value cannot carry every name. The adapter unescapes it |
| A malformed name | Becomes an empty name. The session still validates, so a bad name never blocks chat |
| No header (an older astro-server, or a bearer-token caller) | An empty name, as before |

The name rides on the existing `Session.Username`, so every path that already copied it onto `pb.User` (send, stop, audio) now carries it. The thread-history endpoint returns it too, and only to the user who owns the conversation.

# Migration

None. `user.username` is an existing field, and agents that do not read it see no change. Until astro-server sends the header, web chat names stay empty.
