# Summary

The slack adapter forwarded the raw Slack `U…` id on `msg.User.Id` while the web adapter forwards the canonical WorkOS user_id. Downstream consumers (agents, observability, conversation cache) saw two different identity shapes depending on the source platform, and there was no path to attribute a slack message back to a specific WorkOS user even though the server already had to resolve one to authorize the request.

This change makes the slack adapter converge on the same identity shape as web: when the server resolves the slack user to a linked WorkOS user during the authz call, the adapter rewrites `msg.User.Id` to that canonical id before dispatching.

# Design

- **`Authorizer.Authorize` returns a `Result`.** The interface was `Allowed(...) (bool, error)`; it's now `Authorize(...) (Result, error)` where `Result{Allowed bool, UserID string}`. The boolean still gates the request; the new `UserID` carries the canonical WorkOS user_id when the server could resolve one — echoed back for `identity_type=user`, looked up via `slack_identity_mappings` for `identity_type=slack`, empty otherwise.
- **Cache stores the user_id.** Cache hits now replay the resolved user_id without a server round-trip, so a chatty slack thread doesn't repay the mapping lookup on every message.
- **Slack `dispatch` rewrites the principal.** On allow with a non-empty resolved user_id, `dispatch` sets `msg.User.Id = workosUserID` and stashes the raw slack id on `platform_data["slack_user_id"]` so platform-specific callers (e.g. sending a DM back) can still recover it.
- **Unmapped users untouched.** Slack identities with no `slack_identity_mappings` row leave `msg.User.Id` as the raw slack id — that's the only identity we have for them. Anyone-bypass and observe-channel paths skip authz entirely, so no rewrite either.
- **Web adapter is a passthrough rename.** Web already sends a WorkOS user_id; the new `Result.UserID` will be the same value the handler sent in. No semantic change.

# Migration

Internal interface change. No external consumer touches `authz.Authorizer` directly, so no downstream migration is needed. Agents reading `msg.User.Id` for slack traffic now receive the WorkOS user_id when the slack user has linked their identity, and the raw slack id remains available under `platform_data["slack_user_id"]`.
