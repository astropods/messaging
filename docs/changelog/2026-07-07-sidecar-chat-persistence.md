# Summary

Moves deployment web-chat persistence into the messaging sidecar. Chat
conversations and messages now live in a deployment-local SQLite store instead
of astro-server's central RDS, keeping chat metadata and message bodies out of
the platform database (GDPR data-minimization). The sidecar also owns the
"stop generating" action so a cancelled turn ends server-side, not just in the
browser.

# Design

A new `internal/store/sqlite` store holds conversations and messages
(contiguous per-conversation seq). The web adapter serves the chat-page
contract, which astro-server proxies verbatim at
`/deployments/{id}/chat/...`:

- `GET /api/chat/conversations` — list.
- `GET /api/chat/conversations/{id}` — thread (paginated).
- `POST /api/chat/conversations/{id}/title` — rename only. Scoped to an
  existing, caller-owned conversation; it cannot create a conversation or touch
  messages (replaces the earlier overloaded `PUT .../{id}`).
- `DELETE /api/chat/conversations/{id}` — soft delete.

Persistence is keyed by owner: user turns persist on send (the store creates
and titles the conversation on first send), assistant turns on the terminal
stream chunk. The store (SQLite WAL) lives at `CHAT_DB_PATH` on the agent's
default persistent disk — the 5Gi ReadWriteOnce volume every deployment now
gets at `/data`, shared with the co-located sidecar via `subPath: messaging`
(same pod, so RWO is sufficient — no ReadWriteMany). History survives pod
reschedules; the PVC is retained across redeploys. WAL is single-writer, which
matches agents being single-replica by default (`replicas > 1` is opt-in via
`agent.distributed`), so concurrent multi-pod chat is out of scope. Unset
`CHAT_DB_PATH` disables persistence (local dev). There is no Langfuse coupling —
the sidecar has no Langfuse access.

Stop generating: `POST /api/chat/conversations/{id}/cancel` marks the turn
stopped (a per-turn tracker drops the agent's remaining chunks so a
non-cooperating agent's late reply can't overwrite what the user saw),
persists the partial and flips the conversation out of the streaming state
(never shrinking a finished reply), emits a terminal finish SSE event, and
forwards a `StreamControl{STOP}` to the agent as a best-effort signal for SDKs
that honor it. The gate stays closed until the agent begins a new turn (a
`START` chunk), so a stopped turn's trailing output can't bleed into the next
message.

# Known limitations

Chat history is per-pod, not global. The default disk is ReadWriteOnce and each
StatefulSet replica gets its own PVC (`volumeClaimTemplates`), so every replica
has its own `chat.db`. That is fine at the single-replica default, but a
distributed agent (`agent.distributed`, `replicas > 1`) would fragment
conversations across replicas with no session affinity — whether a thread
persists or is readable depends on which pod served the request. Global
multi-replica chat would need a shared/RWX store or pinning a conversation to a
fixed replica, and is out of scope here.

# Migration

None for agents, and no volume configuration: the durable `/data` disk is
already provisioned for every deployment by the platform's default shared-disk
work, so this feature only sets `CHAT_DB_PATH` (via astro-server's spec
applier); unset is a no-op. Requires the companion astro monorepo change that
proxies `/chat/*` to the sidecar and drops the RDS chat tables.
