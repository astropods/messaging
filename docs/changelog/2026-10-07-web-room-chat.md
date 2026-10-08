# Summary

A chat opened from an Astropods room has to carry the room's scope and a grant on every turn, as a room task does, so the agent's room calls work in chat. Room tasks reach the agent over the agent mesh, which brings the scope and a grant. A room chat stays a web chat, so the web adapter now carries them itself.

# Design

astro-server's chat proxy checks that the person and the agent are both in the room, then gets a grant from the mesh gateway. It sets two headers on the request it forwards. The proxy never forwards these headers from a client.

| Header | Holds |
|---|---|
| `X-Astro-Room-Id` | The room, which is the agent mesh scope |
| `X-Astro-Room-Grant` | The gateway-signed grant for this agent in that room |

The web adapter uses them as follows:

| Step | Behavior |
|---|---|
| Create, or the first send to a new conversation | Binds the conversation to the room, in the new `conversations.room_id` column (`Store.BindRoom`). A conversation that already has messages cannot be bound |
| Send | The room must match the conversation's binding, or the send answers `409 room_mismatch`. A personal chat sent with a room header is refused the same way, and so is a room chat sent without one |
| Turn | The agent message carries `mesh_scope` in its platform data, as the Mesh adapter sets it |
| Grant | The latest grant is kept per conversation, in memory. `GetRoomGrant(conversation)` returns it while it is live, so `RoomClient` works in chat exactly as in a task. Storing a grant drops every entry whose grant has expired, so the map holds only chats active within the grant's lifetime |
| List | `GET /api/chat/conversations?room_id=R` lists that room's chats. Without `room_id` the list leaves room chats out |

The grant type and its expiry check moved from the Mesh adapter to `internal/roomgrant`, which both adapters and the gRPC server use.

# Migration

None. The `room_id` column is added on start, and existing conversations read as personal chats.
