# Summary

A web chat turn reached the agent with the user's WorkOS id and no name. The web adapter reads the session from headers astro-server's proxy sets, and it was wired with no name header at all, so `Message.user.username` was always empty for web chat.

Part of astropods/astro#3126.

# Design

The web adapter now reads the signed-in user's display name from `X-Astro-User-Name`, beside the `X-Amzn-Oidc-Identity` user ID it already read. astro-server sets both on every proxied request, and its proxy forwards only an allowlist of client headers, so a client cannot set either.

| Piece | Behavior |
|---|---|
| `web.NewAstroServerSessionManager()` | The session manager for astro-server's proxy. `cmd/server` uses it in place of an inline `NewHeaderSessionManager` call, so the wiring is under test |
| `HeaderUserName` | Path-escaped by astro-server, since a header value cannot carry every name. The adapter unescapes it |
| A malformed name | Becomes an empty name. The session still validates, so a bad name never blocks chat |
| No header (an older astro-server, or a bearer-token caller) | An empty name, as before |

The name rides on the existing `Session.Username`, so every path that already copied it onto `pb.User` (send, stop, audio) now carries it. The thread-history endpoint returns it too, and only to the user who owns the conversation.

# Migration

None. An astro-server that does not send the header yet leaves the name empty.
