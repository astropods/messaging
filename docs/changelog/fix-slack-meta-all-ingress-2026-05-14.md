# Summary

Some Slack ingress paths forwarded agent payloads **without** the `[slack_meta]` line, so Issueator-style flows could not read `channel_id` / `message_ts` / `permalink` even though the same information existed on the wire (or only inside prose like reaction preambles).

# Design

- **Go Slack adapter** — `handleReactionAdded`, `routeButtonClickToAgent`, and `handleAssistantThreadStarted` now prepend the same `formatSlackMetaLine` JSON line used for mentions and thread replies. Button and assistant-thread messages also set `PlatformContext.message_id` where it was missing.
- **`@astropods/adapter-core` MessagingBridge** — If `platform === "slack"`, content has no `[slack_meta]`, and `platformContext` includes `channelId` + `messageId`, the bridge synthesizes one line before calling the LLM adapter. This covers older messaging builds and any future path that omits the line while still populating proto context.

# Migration

None. Agents should continue to treat `permalink` as optional inside `[slack_meta]`.
