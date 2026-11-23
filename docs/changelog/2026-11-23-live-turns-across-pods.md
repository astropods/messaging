# Summary

Several sidecars sharing one Postgres chat store now behave like one for a live turn. A turn runs in the sidecar that received the send, because each agent replica pairs with its own sidecar. Before this change, everything else about that turn lived only in that sidecar's memory. An SSE stream, a stop, or an interaction answer that reached the other sidecar found nothing. A Slack retry that reached the other sidecar was handled twice.

Nothing changes for a sidecar without `CHAT_DATABASE_URL`.

# Design

Two pieces of shared state carry it: a `turn_owners` table and a NOTIFY bus.

| Need | Mechanism |
|---|---|
| Know which sidecar runs a turn | `turn_owners` row (conversation, pod, heartbeat). A send claims it atomically, or gets `turn_in_progress` when another sidecar holds a live claim. The owner heartbeats every 10 s and releases a row when its turn ends. A row is live for 30 s after its last heartbeat (`chatstore.TurnOwnerTTL`). Shutdown releases nothing, so a draining sidecar keeps its turns until it exits. |
| Show a remote turn as streaming | The conversation list and conversation reads count a live remote owner as streaming. |
| SSE on the other sidecar | A watcher sends `web.watch` with `replay` to the owner, which always answers with the live turn's buffered events, even none. The owner then relays every broadcast and close while the watch is renewed every 10 s. The watcher mirrors them with the owner's event ids, so a `Last-Event-ID` resume works on either sidecar. A watch starts when a stream opens during a remote turn, when the owner announces `web.turn_started` for a conversation the watcher has a stream on, and on the renew tick for any connected conversation with a remote owner. A watch names its owner: the watcher mirrors only that sidecar's events, and a turn on a different sidecar starts a fresh watch with a replay. `web.turn_started`, a close, and a renew tick with no live owner each clear the watch, so the next turn always asks for a replay. |
| A relayed event is lost | The watcher sees a hole in the owner's ids. It sends its live connections a finish and closes them, so the client reconciles from the store instead of rendering around the hole. |
| The owner is gone | The renew tick finds no live owner row for a watched conversation, because the owner crashed or its close was lost. If the mirrored turn had not finished, the watcher sends its live connections a finish and closes them, instead of leaving the client waiting for its stall timer. After a finished turn the streams stay open for the next one. |
| Stop on the other sidecar | `web.stop` to the owner, which runs the same stop as a local one and answers `web.stopped`. Without that answer in 2 s, the receiver stops the turn itself. |
| Interaction answer on the other sidecar | The receiver records the answer in the shared store, then sends `web.answer` to the owner, which advances the turn, delivers to its agent, and answers `web.answered`. Without that answer in 2 s, the receiver returns 503. A retry finds the answer already recorded and forwards it again while the owner is live. The owner applies each interaction's answer once, so a repeat only gets the confirmation. |
| Slack Stop button | The receiver relays `slack.session_stopped`. Only the sidecar streaming or holding that thread's session applies it. |
| Slack retries | Each Events API `event_id` is claimed in `slack_events` as it arrives. A later claim loses and the event is dropped. Claims are kept an hour. |
| Turns a dead sidecar left | At startup, a sidecar treats rows under its own pod id as its previous run's. Every minute, each sidecar also reaps user-last turns that no live owner holds. The grace for a turn with no owner row is one minute. The owner's heartbeat now protects a turn waiting on an interaction, so this replaces the pending-interaction exclusion, which would keep a dead sidecar's waiting turn streaming forever. |

The bus (`internal/cluster`) is one LISTEN connection and one publisher connection per sidecar. A single publisher keeps every sidecar's messages in the order it sent them. The connection manager queues relays under its own lock, so a replay can't be overtaken by a later event. A message over NOTIFY's 8000-byte limit is base64-encoded and split into fragments, which need no JSON escaping, so every fragment fits. A sidecar ignores its own messages, and a relay costs nothing while no other sidecar watches the conversation.

Two more fixes come with sharing:

- **Agent history.** Each agent replica remembers only the turns it answered. `conversations.agent_pod` records which sidecar's agent wrote the last reply. A user turn carries the active branch as `Message.history` when that was another sidecar, as well as on an edit or branch switch. A turn on the sidecar that answered last carries none, so the agent keeps its own memory.
- **Elastic volumes.** EFS reports about 8 EiB through `statfs`. A volume of 1 PiB or more now reports no usage, so the files API skips capacity checks and the usage readout says unavailable.

A lookup failure on `turn_owners` reads as "no remote owner", so the request is handled locally.

# Migration

None until astro-server sets `CHAT_DATABASE_URL`. The first start on this build applies schema version 2 (`turn_owners`, `slack_events`, `conversations.agent_pod`). SQLite gets the `agent_pod` column too, unused there. `POD_NAME` names the sidecar and defaults to the hostname, which is the pod name in Kubernetes.

Slack thread history is still per sidecar. It refetches from Slack when missing or older than 5 minutes, so an agent can miss a turn the other sidecar handled in that window.
