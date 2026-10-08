# Summary

Users can edit a message they sent in the web chat and get a new reply from that point. The old message and its replies stay reachable as another branch. The chat store was append-only and linear, and the agent keeps its own memory per conversation, so an edit needed a branching store, an API to create and switch branches, and a way to tell the agent that its memory no longer matches the conversation.

# Design

- **A conversation is a tree.** `messages.parent_id` holds a message's parent, and `conversations.head_id` holds the newest message of the active branch. Both use an empty value for "linear": an empty `parent_id` means the previous message by `seq`, and an empty `head_id` means the newest message. Rows from before this change need no backfill and read as one linear branch. `root` in `parent_id` marks an edit of the first message. A child always has a higher `seq` than its parent, so the newest message in a subtree is a leaf.

- **A switched head expires when anything is appended after it.** Every append resets `head_id`, and a switch also records the newest message at that moment in `conversations.head_newest_id`. If the newest message has changed since, something appended without resetting the head, and `PageMessages` follows the newest message instead. That is what an older image does after a rollback, so after a roll-forward the conversation shows its newest message and loses only a branch selection made before the rollback.

- **The tree is built in Go.** One skeleton read (`id`, `parent_id`, `seq`, `role`) per request builds it, which is cheap at the 1,000-message cap. `PageMessages` pages along the active branch only, and gives each message with alternatives a `Branches` list.

- **An edit is a send with `edit_of`.** `POST /api/conversations/{id}/messages` accepts the id of a user message. The new message gets the same parent and becomes the head; nothing is deleted. A bad id returns 400 `invalid_edit` before the agent runs.

- **A send names the branch it continues.** The optional `parent_id` is the newest message the client shows. When it is not the active branch's newest message, because another tab switched or sent, the send returns 409 `stale_branch` and writes nothing. A client that omits it keeps the old behavior.

- **`PUT /api/chat/conversations/{id}/branch`** with `{"message_id"}` makes the newest message under that message the head and returns the thread in the `GET` shape. It returns 404 for an unknown message or a foreign conversation.

- **One send or switch at a time.** A send claims the conversation before it checks or writes anything, and holds the claim until the turn is tracked. A branch switch takes the same claim. Either one returns 409 `turn_in_progress` while the other holds it, which closes the window between the in-flight check and the start of the turn.

- **Edits are refused for an agent that cannot honor them.** `edit_of` and `PUT .../branch` return 400 `edit_unsupported` unless the sidecar persists chat and the agent sets `supports_history`. `GET /api/agent/config` reports the same condition as `capabilities.edit`, the way `capabilities.files` works.

- **The agent gets the branch when its memory may be stale.** `conversations.agent_head_id` records the newest reply the agent completed. It moves only on the reply's `END` chunk: progressive writes, a stopped turn's partial, and an errored turn's empty row leave it alone, since the agent may not keep any of them. A send carries the new `Message.history` when its parent is not that reply, and always for an edit, since the agent may hold the edited message even when it never answered it. History is the active branch before the message: user and assistant turns with content, the newest that fit in 512 KiB, oldest first. A user turn with only files becomes a line naming them, so the turn is not lost. An empty `agent_head_id` reads as "the newest message", and the first send pins it, so an unanswered first turn cannot read as held. `root` there means the agent holds nothing.

- **A saved copy replays to the agent.** The agent never wrote a copy's turns under that conversation id. A new copy and a `REPLACE` set `agent_head_id` to `root`, and an `APPEND` does so when it is unset, so the next send carries the transcript. A save also resets `head_id`, so an `APPEND` after a branch switch shows the appended messages.

- **The in-memory thread store follows the branch.** When a send carries history, the thread `GetThreadHistory` serves is rebuilt from it before the message is forwarded. A synchronous transport such as AgentCore routes the whole reply before the forward returns, so the reply lands after the rebuild instead of being wiped by it.

The web adapter's route table moved into `routes()` so tests can serve requests through the real mux.

# Migration

Additive. Existing volumes gain `messages.parent_id`, `conversations.head_id`, `conversations.head_newest_id`, and `conversations.agent_head_id` on next start. Existing conversations read as one branch.

The SDKs move to `0.3.0`: `@astropods/messaging` and `astropods-messaging` carry `Message.history`, `ConversationHistory`, `HistoryMessage`, and `AgentConfig.supports_history`.

Agents are unaffected until they set `supports_history`. To support edits, an agent reads `Message.history` when it is set, replaces what it stored for `conversation_id` with it, and then handles the message. The packaged adapters in `astropods/adapters` do this.

Rolling back to an older image keeps chat working: it writes linear rows, which this image reads correctly. Branches created before the rollback stay in the tree, and the older image shows every row by `seq`, so it interleaves them. After the roll-forward the conversation follows its newest message, and the next send replays history to the agent.
