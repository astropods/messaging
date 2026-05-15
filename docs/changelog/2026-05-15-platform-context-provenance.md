# Summary

Refines `PlatformContext` with a fine-grained event taxonomy (`event_kind`) and a clean "is this in a thread?" signal (`thread_root_id`). Retires the redundant `trigger` field introduced in the observe-channels PR — `event_kind` subsumes it.

# Design

- **`PlatformContext.event_kind`** — enum naming the platform event that produced this message: `DM`, `APP_MENTION`, `THREAD_REPLY`, `OBSERVED`, `REACTION`, `BUTTON_CLICK`, `SLASH_COMMAND`, `ASSISTANT_THREAD_STARTED`. Set on every outbound message by every adapter. Lets agents branch on the source event without inspecting content — including detecting `@`-mentions even though the adapter strips the `<@bot>` token.
- **`PlatformContext.thread_root_id`** — the parent thread's root timestamp when this message is a reply *inside* an existing thread; empty for top-level messages. Distinct from `thread_id`, which remains the agent's reply target (set even for top-level messages whose response should open a new thread).
- **`Trigger` removed.** The DIRECT/OBSERVED bit is a one-bit projection of `event_kind` (`OBSERVED ⇔ EVENT_KIND_OBSERVED`). Carrying both invited drift; `event_kind` is now the single source of truth.
- **Reaction handler** upgraded from `fetchMessageText` to `fetchReactionMessage` so reactions on thread replies get the correct `thread_root_id` and the agent's reply targets the parent thread.
- **Button click** path now populates `message_id` (was previously empty).
- **Central log** (`internal/grpc/server.go::logIncomingMessage`) emits `event_kind` and `thread_root_id` instead of `trigger`.

# Agent decision matrix

| Question | Answer |
|---|---|
| Was I `@`-mentioned? | `EventKind == EVENT_KIND_APP_MENTION` |
| Was this observed? | `EventKind == EVENT_KIND_OBSERVED` |
| Is this in a thread? | `ThreadRootId != ""` |
| Was I mentioned inside a thread? | `EventKind == EVENT_KIND_APP_MENTION && ThreadRootId != ""` |

# Migration

`Trigger` was unreleased. No external consumers. Agents reading `platformContext.trigger` should switch to `event_kind`. TypeScript SDK exports `PlatformContextEventKind` (string union); Python `message_pb2.PlatformContext.EventKind` is a regular proto enum.
