# Summary

The slack adapter forwarded the raw Slack `U…` id on `msg.User.Id` while the web adapter forwards the canonical WorkOS user_id. Downstream consumers (agents, observability, conversation cache) saw two different identity shapes depending on the source platform; there was no path to attribute a slack message back to a specific WorkOS user even though the server already had to resolve one to authorize the request; and unlinked Slack users disappeared into a single aggregated bucket on the Insights page because their raw slack ids were indistinguishable from arbitrary unknown identifiers.

This change makes the slack adapter rewrite `msg.User.Id` on every allowed dispatch to a canonical trace identity — the resolved WorkOS user_id when the Slack user has linked, otherwise a workspace-qualified `slack:<team>:<user>` form so unlinked users remain identifiable per-id on Insights instead of collapsing into "Unidentified" / "Unattributed".

# Design

- **`Authorizer.Authorize` returns a richer `Result`.** The interface was `Allowed(...) (bool, error)`; it's now `Authorize(...) (Result, error)` where `Result{Allowed bool, UserID, SlackUserID, SlackTeamID string}`. The boolean still gates the request; `UserID` carries the canonical WorkOS user_id when the server could resolve one (echoed back for `identity_type=user`, looked up via `slack_identity_mappings` for `identity_type=slack`, empty when no mapping exists); `SlackUserID` / `SlackTeamID` echo the input slack identity on every allowed slack request so the adapter can build a namespaced fallback when no WorkOS user resolved.
- **Cache stores the full Result.** Cache hits replay every identity field without a server round-trip, so a chatty slack thread doesn't repay the mapping lookup on every message.
- **Every request hits the server.** The token's `anyone_adapters` claim is no longer a fast-path bypass — it's preserved only as a degraded-mode fallback when the server is unreachable. The fast-path bypass would skip resolution entirely, defeating the whole point of plumbing identity through.
- **Slack `dispatch` always rewrites the principal on allow.** It calls `canonicalUserID(result, teamID, msg.User.Id)` and assigns the result to `msg.User.Id`. The helper prefers `result.UserID` (linked user → WorkOS id), then falls back to `result.SlackTeamID`+`result.SlackUserID` to namespace as `slack:<team>:<user>`, then to the input `teamID`+`slackUserID` (degraded mode / pre-team_id callers). With no team available anywhere it produces a bare `slack:<user>` as a last resort. The raw incoming slack id is *not* stashed on `platform_data` — agents that need the unmapped slack id can parse it out of the namespaced form, and the workspace context is already on `PlatformContext` for senders that need it.
- **Observe channels skip rewrite too.** Passive watch channels don't run authz, so they also keep `msg.User.Id` as the raw incoming value — attributing observed traffic to the observed user on a trace would misrepresent intent.
- **Web adapter is a passthrough.** Web already sends a WorkOS user_id; `Result.UserID` is the same value the handler sent in. No semantic change.

# Migration

Internal interface change. No external consumer touches `authz.Authorizer` directly, so no downstream migration is needed inside this repo.

The wire format `msg.User.Id` now carries one of: `user_01…` (linked WorkOS user), `slack:<team>:<user>` (unlinked Slack user, namespaced), bare `U…` (historical unlinked, pre-namespacing), or empty (anonymous). The astro Insights page (`apps/astro-client/src/components/activity/user-classification.ts`) parses these forms via `isSlackUserId()` to render unlinked Slack users on their own "Slack user - U07…" rows; changing either side of that contract requires a coordinated update.

Agents reading `msg.User.Id` for slack traffic now receive the WorkOS user_id when the slack user has linked, otherwise the namespaced `slack:<team>:<user>` form. Agents that previously read the raw slack id from `platform_data["slack_user_id"]` should either parse it out of the namespaced form or read it directly from the slack-specific fields on `PlatformContext` — that key is no longer set.
