## Summary

Slack messages continue to pass deterministic workspace identity to Astro Server without doing per-message Slack profile enrichment in deployed agents.

## Design

The Slack adapter stamps the Slack team ID onto the message platform context and sends it as `identity_scope` when calling the authorization endpoint. Astro Server can pair that scope with the raw Slack user ID to record live usage and build workspace-scoped deep links.

Messaging no longer calls Slack `users.info`, no longer caches Slack profiles, and no longer appends Slack profile fields to authorization query params. Slack names and avatars are synced by Astro Server when an Astro user connects a Slack workspace through the account Slack OAuth flow.

## Migration

No deployed Slack agent scope change is required for profile enrichment. Deploy this with the Astro Server change that syncs Slack workspace directories at account-connect time.
