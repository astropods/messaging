# Summary

The agent mesh now partitions an account into scopes, one per room, and the gateway rejects any envelope without one. The Mesh adapter carries the scope on everything it sends, and hands the agent the scope grant the room API needs, so an agent can work in several rooms and touch each one only while it has work there.

# Design

**The scope travels with the work.** A task or message the adapter receives names its scope. The adapter keeps it on the conversation and stamps it on every status and reply it sends back. The agent sees it as `mesh_scope` in the message's platform data.

| Agent mesh frame | Adapter |
|---|---|
| `deliver` of a task offer | Records the scope; claims the task |
| `granted` (claim reply) | Records the grant for the conversation |
| `granted` without `re` (refresh) | Replaces the conversation's grant |
| `deliver` of a message | Records the scope and the message's grant |

**The agent asks for the grant when it needs it.** A grant expires after a few minutes, and the gateway refreshes it while the task's lease lives, so the agent never stores one. A new unary RPC returns the conversation's current grant:

```proto
rpc GetRoomGrant(RoomGrantRequest) returns (RoomGrantResponse);
// RoomGrantResponse: found, room_id, grant, expires_at, api_url
```

`api_url` is astro-server's base URL, read from the deploy token's `iss`, so the SDK never parses a token. The SDKs wrap it in a room client that fetches the grant before every call and sends uploads straight to astro-server:

```ts
const room = new RoomClient(client, message.conversationId);
await room.uploadDocument('q3-summary.md', summary, 'text/markdown');
```

```python
room = RoomClient(stub, message.conversation_id)
room.upload_document("q3-summary.md", summary, "text/markdown")
```

A conversation that is not a mesh task or message, or whose task has ended, has no grant, and the room client raises a 403.

# Migration

None for agents that do not use rooms. An agent on the Mesh adapter needs this release once the gateway enforces scopes, because envelopes without a scope are rejected.
