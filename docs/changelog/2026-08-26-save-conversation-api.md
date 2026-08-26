# Summary

Agents can now copy a conversation from somewhere else into a user's Astro chat history. The motivating case is Slack: a user asks the agent in a thread to save it, and the thread shows up in their private chat page alongside conversations they started there.

Nothing about this is Slack-specific and nothing happens automatically. The platform gained one payload, `SaveConversation`, and the agent decides when to send it, for whom, and what goes in it. An agent could just as well seed a conversation from an email chain, a Linear issue, or a nightly digest.

This also fixes a live double-delivery: an `@`-mention posted inside a channel thread reached the agent twice.

# Design

- **`SaveConversation` is a unary RPC**, not a stream payload. The agent has to see what happened to an existing copy before deciding what to do next, and a one-way `AgentResponse` gives it nowhere to learn that. The response carries the conversation id and one of `CREATED`, `REPLACED`, `APPENDED`, `SKIPPED_DELETED`, `SKIPPED_DIVERGED`, `SKIPPED_CONFLICT`.

- **The conversation id is derived, not allocated.** It is a UUIDv5 over `(user_id, idempotency_key)`. Deriving means the agent can link to the copy without a round trip, a repeat save resolves to the same row after a restart, and the id is still a UUID, so nothing upstream that validates conversation ids has to change. The namespace UUID is fixed forever; changing it would orphan every copy already saved.

- **A repeat save refreshes a copy the user has not touched.** Refreshing rather than appending is what makes the honest implementation the lazy one: an agent that re-reads its whole source and re-sends it propagates edits and deletions for free. An append-only API would have pushed builders toward incremental writes where a deletion at the source never lands.

- **The platform never destroys turns it did not write.** A saved copy is an ordinary conversation, so the user can chat in it. Once they have, refreshing it would silently delete their own messages, an answered form, and the agent's replies to them. Under the default `SKIP` the save reports `SKIPPED_DIVERGED` and changes nothing. A new `messages.origin` column is what lets a save tell its own rows from the user's.

- **The agent picks the policy with `on_conflict`.** `SKIP` (default) refreshes a pristine copy and leaves a diverged one alone. `APPEND` adds the given messages after whatever is there, which is how an agent syncs incrementally. `REPLACE` overwrites regardless. Destroying the user's turns stays possible, but only when the agent says so in the call.

- **A deleted copy is never recreated.** Without this a user could not escape an agent that saves on every source message: they delete it, the next message brings it back. `SaveConversation` reports `saved=false` for a soft-deleted id, which matches how `EnsureForSend` already refuses to revive one.

- **`user_id` must carry the `user_` prefix.** A raw Slack id or an arbitrary string would write a conversation no session can ever open. Validation failures return `InvalidArgument` rather than being swallowed: on a unary call the agent can see and handle them, which was not true of the stream payload.

- **A copy never reads as a turn in flight.** A Slack thread ends on a human turn, and `assistant_streaming` is derived from "the last message is the user's". Without a guard every saved copy would spin in the sidebar forever, and the startup reaper would append a terminal empty assistant row to each one on every restart. Both now skip rows written by a save.

- **Four additive columns**, all via `ensureColumn` on existing volumes: `conversations.source_label`, `conversations.source_url`, `messages.author`, and `messages.origin`. `author` matters because every human turn in a copied-in multi-party thread is role `user`, so without it the transcript reads as though one person said everything. All three are surfaced on the chat JSON as `source_label`, `source_url`, and `author`.

- **Threaded mentions no longer double-deliver.** Slack sends both `message.channels` and `app_mention` for a mention. The guard that left mentions to the `app_mention` path only ran for top-level posts, so a mention inside a thread fell through as an ordinary thread reply and dispatched a second time, with both turns sharing one entry in `contentBuffers`. The guard now covers any channel message. DMs are deliberately excluded: a DM mention has no `app_mention` counterpart to fall back on.

# Migration

Additive. Existing volumes gain the three columns on next start, and conversations created before this change read back with the new fields empty.

Agents that do not call `SaveConversation` are unaffected. Agents that do need `CHAT_DB_PATH` set; the call returns `FailedPrecondition` when chat persistence is disabled.

The dedup change alters what reaches agents in one case: an `@`-mention inside a channel thread now arrives once (as `EVENT_KIND_APP_MENTION`) instead of twice. An agent that happened to depend on seeing the `EVENT_KIND_THREAD_REPLY` copy will stop receiving it.
