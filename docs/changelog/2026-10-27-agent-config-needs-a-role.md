# Summary

`GET /api/agent/config` returned the agent's system prompt and tools to anyone the web chat grants admit. An agent whose web chat admits everyone in the org showed its system prompt to all of them, including members with no role on the agent. Astropods' own proxy already trims the config for them, but the agent's chat host serves this route directly.

This leaves the system prompt and tools out for a caller who holds no role on the agent. Part of astropods/astro#3557, which makes the authorize callback report the role.

# Design

The authorize callback answers an allowed web user with `can_read_deployment`. `authz.Result` carries it as `CanReadDeployment`, cached with the decision for 60 seconds. `HandleAgentConfig` answers a caller without a role with the capabilities alone:

```json
{ "capabilities": { "files": true, "edit": true } }
```

The capabilities stay because every chat reads them to offer attachments, Edit, and the branch picker, a chat-only viewer's included.

The other cases:

| Case | Role |
|---|---|
| Server sends `can_read_deployment` | Its value |
| Server predates the field | Has one, as before |
| Degraded fallback (server unreachable, `anyone_adapters`) | None |
| `AllowAll` (no `ASTRO_AUTHZ_TOKEN`, local dev) | Has one |
| No authorizer wired | Has one |

# Migration

None. The two changes ship in either order. An older astro-server sends no field, so this sidecar keeps serving the whole config. An older sidecar ignores the field. An agent is covered once it is redeployed onto this sidecar and astro-server sends the field.
