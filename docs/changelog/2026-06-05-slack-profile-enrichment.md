## Summary

Slack messages can now carry best-effort Slack profile metadata into Astro's authorization callback. This lets downstream Insights UI render unlinked Slack users with Slack-sourced display names, usernames, and avatars instead of only raw user IDs.

## Design

The Slack adapter resolves `users.info` with the configured bot token before calling the existing authorization endpoint. The lookup is cached per `(team_id, slack_user_id)` so chatty Slack threads do not add a Slack API call per message.

Profile metadata is optional and never affects authorization. Missing `users:read`, Slack API failures, and old server versions continue through the same raw Slack ID path. The authz client only appends profile query parameters when Slack returned a profile.

## Migration

Slack apps should include the `users:read` bot scope for profile enrichment. No email scope is required.
