# Summary

An agent listed its mesh skills in code, through `AgentConfig.skills`. That list came from the agent at runtime, so the platform advertised whatever the agent claimed, and the framework adapters had no way to set it anyway. Skills are now declared in `astropods.yml` (`agent.skills`), where astro-spec validates them at registration. astro-server hands them to this sidecar as `MESH_SKILLS`, and the Mesh adapter advertises only those, with their descriptions.

# Design

```
MESH_SKILLS='[{"name":"github.issue.investigate","description":"Investigates a new GitHub issue."}]'
```

The card's skills are the agent's identity (`agent.sasbot`), then `MESH_SKILLS` in order, keeping the first entry for each name. `AgentConfig.skills` is ignored, and the adapter no longer watches the agent's config or rejoins when it changes, because nothing it advertises can change at runtime. The proto field and the Node SDK type stay for wire compatibility, marked deprecated.

Each skill on the card now carries its `description`, which the card dropped before. A name that breaks the skill pattern or uses the reserved `agent.` prefix is skipped with a warning. A `MESH_SKILLS` value that is not a JSON list of skills is ignored with a warning, and the sidecar starts as before.

# Migration

An agent that set `AgentConfig.skills` in code loses those skills on the mesh until it lists them under `agent.skills` in `astropods.yml` and is redeployed. Its `agent.<name>` skill is unaffected.
