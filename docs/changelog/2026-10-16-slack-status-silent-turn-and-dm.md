# Summary

Two bugs in the Slack thread status indicator:

- **A silent turn left "thinking" up forever.** On a mention, the sidecar sets "Assistant is thinking..." before it dispatches the message. Slack clears that status by itself only when the bot replies. An agent that ends its turn with no content, for example to stay quiet in a channel, left the indicator spinning with no way to stop it.
- **Status updates failed in top-level DMs.** A top-level DM has no thread timestamp, so every status update sent `assistant.threads.setStatus` an empty `thread_ts`, and Slack rejected it with `invalid_thread_ts`.

# Design

| Situation | Before | After |
|---|---|---|
| Turn ends with no content, in a thread | Status left set | Status cleared with an empty status |
| Turn ends with no content, in a top-level DM | Nothing | Nothing (no thread to clear) |
| Status update in a top-level DM | `invalid_thread_ts` error | Skipped, with a debug log |
| Status update in a thread | Sent | Sent, unchanged |

Both checks live in `ai_features.go`. `setSlackStatus` returns early when the conversation has no thread timestamp. The empty-content branch of the END chunk calls `clearSlackStatus`, which skips a conversation without a thread and logs a failed clear as a warning rather than failing the turn.

# Migration

None. An agent that ends a turn with no content in a thread now clears the indicator. A workaround that sends an empty custom status before finishing still works, and is no longer needed.
