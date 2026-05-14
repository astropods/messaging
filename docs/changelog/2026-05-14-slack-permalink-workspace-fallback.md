# Summary

When `chat.getPermalink` fails or returns no URL, `[slack_meta]` still receives a stable Slack message link built from the workspace base URL reported by `auth.test`.

# Design

- **Init** — `Initialize` always runs `auth.test` once. On success, `URL` is stored (trimmed, no trailing slash) on the adapter. Existing `channel_messages` bot user resolution reuses the same response instead of a second `auth.test`.
- **Fallback** — After `GetPermalinkContext`, if `permalink` is still empty and we have `workspaceURL`, `formatSlackMetaLine` sets `permalink` to `{workspaceURL}/archives/{channel}/p{ts_without_dot}` (same shape as Slack archive links). API permalinks still win when present.

# Migration

None. Agents may rely on `permalink` in `[slack_meta]` when present; it may now be either an API permalink or this workspace-scoped archive URL.
