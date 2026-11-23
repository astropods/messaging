# Summary

Several sidecars sharing one Postgres chat store now behave like one for a live turn. A turn runs in the sidecar that received the send, because each agent replica pairs with its own sidecar. Before this change, everything else about that turn lived only in that sidecar's memory. An SSE stream, a stop, or an interaction answer that reached the other sidecar found nothing. A Slack retry that reached the other sidecar was handled twice.

Nothing changes for a sidecar without `CHAT_DATABASE_URL`.

# Design

Two pieces of shared state carry it: a `turn_owners` table and a NOTIFY bus.

| Need | Mechanism |
|---|---|
| Know which sidecar runs a turn | `turn_owners` row (conversation, pod, heartbeat). A send claims it atomically, or gets `turn_in_progress` when another sidecar holds a live claim. The owner heartbeats every 10 s and releases on turn end. A row is live for 30 s after its last heartbeat (`chatstore.TurnOwnerTTL`). |
| Show a remote turn as streaming | The conversation list and conversation reads count a live remote owner as streaming. |
| SSE on the other sidecar | The subscribing sidecar publishes `web.watch`. The owner replies with its buffered events for the live turn, then relays every later broadcast and close while the watch is renewed. The watcher mirrors them with the owner's event ids, so a `Last-Event-ID` resume works on either sidecar. |
| Stop on the other sidecar | `web.stop` to the owner, which runs the same stop as a local one. |
| Interaction answer on the other sidecar | The receiver records the answer in the shared store, then sends `web.answer` to the owner, which advances the turn and delivers to its agent. |
| Slack Stop button | The receiver relays `slack.session_stopped`. Only the sidecar streaming or holding that thread's session applies it. |
| Slack retries | Each Events API `event_id` is claimed in `slack_events` as it arrives. A later claim loses and the event is dropped. Claims are kept an hour. |
| Startup cleanup | A turn with a live owner is never reaped. The grace for a turn with no owner row drops to one minute. |

The bus (`internal/cluster`) is one LISTEN connection and one publisher connection per sidecar. A single publisher keeps every sidecar's messages in the order it sent them. The connection manager queues relays under its own lock, so a replay can't be overtaken by a later event. Messages over NOTIFY's 8000-byte limit travel as fragments split on rune boundaries. A sidecar ignores its own messages, and a relay costs nothing while no other sidecar watches the conversation.

Two more fixes come with sharing:

- **Agent history.** Each agent replica remembers only the turns it answered, so the shared `agent_head_id` can't say what this agent knows. On a shared store, every user turn carries the active branch as `Message.history`.
- **Elastic volumes.** EFS reports about 8 EiB through `statfs`. A volume of 1 PiB or more now reports no usage, so the files API skips capacity checks and the usage readout says unavailable.

A lookup failure on `turn_owners` reads as "no remote owner", so the request is handled locally.

# Migration

None until astro-server sets `CHAT_DATABASE_URL`. The first start on this build applies schema version 2 (`turn_owners`, `slack_events`). `POD_NAME` names the sidecar and defaults to the hostname, which is the pod name in Kubernetes.

Slack thread history is still per sidecar. It refetches from Slack when missing or older than 5 minutes, so an agent can miss a turn the other sidecar handled in that window.
