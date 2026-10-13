# Summary

The gateway offers a task only to an agent whose card accepts every part type in it. The Mesh adapter's card declared no `accepts`, which the gateway reads as text only. A room task carries a `data` part with its room task ID and inputs, so no agent was ever offered one, and the task waited in the queue until it expired.

# Design

The card now declares `accepts: ["text", "data"]`, the two part types the adapter passes to the agent:

| Card field | Source |
|---|---|
| `accepts` | `text` and `data`. `file` parts are not passed to the agent, so the card leaves them out |

# Migration

Redeploy agents on the new image. Until then, room tasks sent to them stay queued.
