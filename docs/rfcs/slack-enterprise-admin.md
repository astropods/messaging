# RFC: Slack Enterprise Grid Support and Admin-Controlled User Access

**Status:** Draft
**Author:** Rodric Rabbah
**Date:** 2026-04-06
**Branch:** `feature/slack-enterprise-admin`

## Summary

Two related improvements to the Slack adapter: (1) Enterprise Grid support so the bot filters events to a specific org when deployed with an org-level token, and (2) a two-tier access model where a static admin list (configured at deploy time) controls which users can interact with the bot, and admins can grant/revoke access for additional users at runtime via Slack commands.

## Motivation

**Enterprise Grid:** Large organizations run Slack Enterprise Grid, where a single org-level bot token delivers events from all workspaces in the org. Without enterprise_id filtering, a bot deployed for one org could inadvertently process events from another org if the token has broad scope.

**Admin-controlled access:** The current allowlist (`AllowedUserIDs`, `AllowedChannelIDs`) is static — changing it requires a redeploy. Agents often need to onboard users incrementally without ops involvement. Admins need to add or remove users in real time from within Slack itself.

## Design

### Two-tier access model

| Tier | Source | Mutable | Can run admin commands |
|------|--------|---------|----------------------|
| Admins | `SLACK_ADMIN_USER_IDS` env var | No (static) | Yes |
| Dynamic users | Admin-granted at runtime | Yes (persisted to Redis) | No |

`SLACK_ALLOWED_USER_IDS` is accepted as a deprecated alias for `SLACK_ADMIN_USER_IDS`. If both are set, `SLACK_ADMIN_USER_IDS` takes precedence. A deprecation warning is logged when the old name is used. The JSON key in `SLACK_CONFIG` changes from `allowed_user_ids` to `admin_user_ids`; the old key is accepted as fallback with the same deprecation warning.

If `SLACK_ADMIN_USER_IDS` is empty, the bot is open to everyone and admin commands are disabled — there is nothing to manage.

`isAllowed(channelID, userID)` returns true if: the bot is unrestricted (empty admin list) OR the user is an admin OR the user/channel is in the dynamic list. All lookups are O(1) via `map[string]struct{}` rather than O(n) slice scans, and reads are protected by `sync.RWMutex` so concurrent access is safe.

### Admin commands

Admins invoke commands by mentioning the bot. Commands are intercepted before the message reaches the agent.

| Mention text | Effect |
|---|---|
| `@bot allow user @username` | Add user to dynamic list |
| `@bot deny user @username` | Remove user from dynamic list |
| `@bot allow channel #channel` | Add channel to dynamic list |
| `@bot deny channel #channel` | Remove channel from dynamic list |
| `@bot list allowed` | List current dynamic list |

The bot replies in-thread with a confirmation. Unknown commands are forwarded to the agent as normal messages.

### Persistence

Dynamic list changes are persisted to Redis using native SET operations (`slack:allowlist:users`, `slack:allowlist:channels`). The in-memory list is authoritative for reads; Redis is written synchronously on mutation. On startup the adapter loads the persisted list and merges it with the static config. If Redis is unavailable, the dynamic list is in-memory only (data lost on restart).

### Enterprise Grid filtering

A new `SLACK_ENTERPRISE_ID` env var (also settable in `SLACK_CONFIG` JSON) restricts event processing to a specific Slack org. For each incoming `EventTypeEventsAPI` socket event, the raw payload is inspected for `enterprise_id`. If it doesn't match the configured value, the event is dropped. No-op when `EnterpriseID` is not configured.

## Implementation

### Config changes

`SlackAdapterConfig` gains:
- `AdminUserIDs []string` (JSON key `admin_user_ids`; fallback to `allowed_user_ids` with deprecation warning)
- `EnterpriseID string` (JSON key `enterprise_id`)

New env vars: `SLACK_ADMIN_USER_IDS` (primary), `SLACK_ENTERPRISE_ID`. `SLACK_ALLOWED_USER_IDS` remains functional as a deprecated fallback for `SLACK_ADMIN_USER_IDS`. Both `AdminUserIDs` and `EnterpriseID` are propagated to `adapter.Config`.

### New files

**`internal/adapter/slack/allowlist.go`**

- `allowList` struct: `adminIDs map[string]struct{}` (static), `userIDs map[string]struct{}` (dynamic), `chanIDs map[string]struct{}` (dynamic), `mu sync.RWMutex`, `store AllowListStore`
- `AllowListStore` interface: `LoadUsers`, `LoadChannels`, `AddUser`, `RemoveUser`, `AddChannel`, `RemoveChannel`
- `RedisAllowListStore` implementation using Redis SET commands
- `newAllowList(adminIDs []string, store AllowListStore)` loads persisted state on construction

**`internal/adapter/slack/admin.go`**

- `parseAdminCommand(text string) (cmd, targetID string)` — strips bot mention, identifies command verb and target from `<@USERID>` / `<#CHANID|name>` patterns
- `(a *SlackAdapter) handleAdminCommand(ctx, channelID, threadID, userID, rawText string) bool` — executes command, replies in-thread, returns true if handled

### Modified files

**`internal/adapter/adapter.go`**: add `EnterpriseID string` to `Config`

**`internal/adapter/slack/adapter.go`**:
- `SlackAdapter` gains `allowList *allowList` field; static `slices.Contains` checks replaced with `allowList` methods
- `Initialize()` constructs the `allowList` from `config.AdminUserIDs` (admins) and calls `load(ctx)` to hydrate from store
- `handleSocketEvent()`: when `EnterpriseID` is set, unmarshal minimal struct from `evt.Request.Payload` to check `enterprise_id` before processing
- `handleAppMention()`: if sender `isAdmin()` and bot `isRestricted()`, try `handleAdminCommand()` first; return without forwarding if handled
- `SetAllowListStore(store AllowListStore)` — called from `main.go` to inject Redis-backed persistence

**`cmd/server/main.go`**: when `cfg.Storage.Type == "redis"`, construct `slack.NewRedisAllowListStore(cfg.Storage.RedisURL)` and call `slackAdapter.SetAllowListStore(...)` after initialization.

## What This Does Not Include

- Slash command support (uses app mentions only)
- Admin-initiated channel restrictions (admins can add channels to the allowlist but cannot restrict the bot to only certain channels at runtime — that remains a config/deploy concern)
- Audit log of admin actions beyond structured log lines
