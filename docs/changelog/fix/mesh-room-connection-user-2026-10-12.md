# Summary

A room task runs with no one chatting, so an agent had no user to fetch a connection token for. The Mesh adapter set the turn's user to the sender's mesh address, which astro-server's token route can't resolve to anyone.

The turn's user is now `room:<scope>` whenever the envelope carries a scope. astro-server resolves that user to the room member who shared the connection with the agent.

# Design

| Envelope | Turn user (`pb.User.Id`) |
|---|---|
| Has a scope (every room task and room message) | `room:<scope>` |
| No scope | `env.From`, as before |

`Username`, `ChannelId`, and the `mesh_from` platform data still carry the sender's address, so replies route as before.

An agent passes the turn's user to `ConnectionClient.getToken` as it does any other user. Neither adapter-core nor the agent changes. astro-server checks that the agent is in the room, that someone in the room shared the connection, and that the agent has a task in progress there.

# Migration

None. Redeploy agents to pick up the new sidecar. An agent's memory resource for room turns changes from the sender's address to `room:<scope>`.
