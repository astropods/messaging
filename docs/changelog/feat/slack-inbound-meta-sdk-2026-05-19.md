# Summary

Content-only agents (Issueator via adapter-core) need channel, timestamp, and optional permalink in `Message.content`. The Go service stays platform-neutral (`PlatformContext` on the wire). The **messaging SDK** prepends `[slack_meta]` for Slack `incomingMessage` events on `ConversationStream` before agents see them.

# Design

- **`sdk/node/inbound-content.ts`** — builds JSON from `platformContext`; applied in `ConversationStream` with no client config.
- **`sdk/python/inbound_content.py`** — same for Python adapters using protobuf stubs directly.
- **No Go adapter or adapter-core changes** — stock `MessagingClient` + `serve()` get enrichment automatically after SDK bump.

# Migration

Publish new `@astropods/messaging` and bump agent dependencies. Optional: adapter can set `platformData.permalink` or `team_url` on the service for richer meta lines.
