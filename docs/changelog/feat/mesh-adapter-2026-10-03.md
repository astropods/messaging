# Summary

Agents could talk to people through Slack and the web chat, but not to each other. The new Mesh adapter connects the sidecar to the Agent Mesh Protocol (AMP) gateway, so other agents in the same account can send this agent tasks and messages. They reach the agent as a normal conversation, and its streamed reply goes back as the task result. An agent needs no code change to be reachable.

The adapter is off unless `MESH_ENABLED=true`. astro-server turns it on, with no `astropods.yml` change.

# Design

**A conversation per task.** The adapter claims a task the gateway offers and forwards it to the agent as a `Message` with platform `mesh` and the task ID as its conversation ID. The server's conversation cache then routes the agent's `AgentResponse` stream back to the adapter, as it does for Slack and web:

| Agent sends | Mesh receives |
|---|---|
| `StatusUpdate` | A `working` status with the status text, sent once per change |
| `ContentChunk` `START`, `DELTA`, `REPLACE` | Buffered |
| `ContentChunk` `END` | A `completed` status with the full reply |
| `ErrorResponse` | A `failed` status with the error message |

A direct mesh message (not a task) gets a mesh message back, with `reply_to` set.

**Identity and settings.** The adapter logs in with the deployment's `ASTRO_AUTHZ_TOKEN` against `ASTRO_MESH_URL`. Its card comes from two places:

| Card field | Source |
|---|---|
| `name` | `MESH_NAME`, the agent's name |
| `skills` | `agent.<MESH_NAME>`, always, plus the agent's `AgentConfig.skills` |
| `accepts` | `text` and `data`, the part types the adapter passes to the agent |
| `max_concurrent` | 4: at most four tasks at once |

The `agent.` prefix is reserved: `skill:agent.sasbot` reaches that agent by name, and a declared skill starting with `agent.` is dropped so one agent cannot answer for another's name. When the agent sends a new `AgentConfig` whose skills differ, the adapter rejoins with the new card (`AgentConfigStore.Changed`).

```ts
client.sendAgentConfig({ systemPrompt, tools, skills: [{ name: "summarize" }] });
```

**Session.** The adapter holds one WebSocket session (subprotocol `amp.v1alpha1`), sends a heartbeat every half interval, and reconnects with backoff from 1 to 30 seconds. A rejected credential retries at the 30-second ceiling. A dropped session forgets its in-flight turns; the gateway re-offers their tasks when their leases expire.

`AgentConfig` gains `skills` (field 4). An older agent that does not set it is reachable as `agent.<name>` only.
