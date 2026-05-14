# Summary

`assistant.threads.setStatus` / `setSuggestedPrompts` only accept **Slack AI Assistant** thread timestamps. Conversations keyed by a normal message ts (for example **emoji reactions** on a channel message) are not assistant threads, so Slack returns `invalid_thread_ts` and the messaging server logged routing errors even though replies still worked via `chat.postMessage`.

# Design

Treat `invalid_thread_ts` from those HTTP APIs as an expected no-op: log at **debug**, return success from `setSlackStatus` / `setSlackPrompts` so agent streaming is not reported as a routing failure.

# Migration

None.
