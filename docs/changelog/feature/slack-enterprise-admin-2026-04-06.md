## Summary

Adds Slack Enterprise Grid org filtering and a two-tier access control model that lets designated admins grant or revoke user access at runtime via Slack commands, without requiring a redeploy.

## Design

### Enterprise Grid filtering

A new `SLACK_ENTERPRISE_ID` env var (or `enterprise_id` in `SLACK_CONFIG` JSON) restricts the bot to a specific Slack org. For each incoming socket event, the raw payload is inspected for `enterprise_id`. Events from other orgs are dropped silently. No-op when not configured.

### Two-tier access control

| Tier | Source | Mutable | Can run admin commands |
|------|--------|---------|----------------------|
| Admins | `SLACK_ADMIN_USER_IDS` (static, from env) | No | Yes |
| Dynamic users/channels | Admin-granted at runtime | Yes (persisted to Redis) | No |

When `SLACK_ADMIN_USER_IDS` is empty the bot is open to everyone and admin commands are disabled — there is nothing to manage.

`isAllowed()` now uses `map[string]struct{}` with `sync.RWMutex` for O(1) concurrent-read-safe lookups, replacing the previous O(n) `slices.Contains` calls on unprotected slices.

Dynamic grants persist to Redis using native SET operations (`slack:allowlist:users`, `slack:allowlist:channels`). The adapter loads the persisted list at startup and merges it with static config. Falls back to in-memory-only if Redis is unavailable.

### Admin commands

Admins invoke commands by @-mentioning the bot. Commands are intercepted before the message reaches the agent; unrecognized text is forwarded to the agent as normal.

```
@bot allow user @username       — grant access
@bot deny user @username        — revoke access
@bot allow channel #channel     — grant channel access
@bot deny channel #channel      — revoke channel access
@bot list allowed               — show current dynamic grants
```

### Deprecation

`SLACK_ALLOWED_USER_IDS` and the `allowed_user_ids` JSON key are accepted as deprecated aliases for `SLACK_ADMIN_USER_IDS` / `admin_user_ids`. A deprecation warning is logged when the old names are used. `SLACK_ALLOWED_USER_IDS` will be removed in a future release.

## Migration

- **No change required** for deployments that don't use `SLACK_ALLOWED_USER_IDS`. The bot behavior is unchanged when no admin list is configured.
- **Rename** `SLACK_ALLOWED_USER_IDS` → `SLACK_ADMIN_USER_IDS` (or `allowed_user_ids` → `admin_user_ids` in `SLACK_CONFIG` JSON) to adopt the new naming. The old name continues to work but logs a deprecation warning.
- **Set `SLACK_ENTERPRISE_ID`** if deploying with an org-level Enterprise Grid token and you want to restrict the bot to a specific org.
- Storage must be `STORAGE_TYPE=redis` for dynamic grants to survive restarts; otherwise they are in-memory only.
