# Summary

An agent can list several models in `astropods.yml`, but a chat user could not choose between them: the deploy picked one model for every turn. With `selection: live` on its model entries, astro-server now sends the list to this sidecar, and the web chat lets a user pick a model for each message. The sidecar checks the pick against the list and tells the agent which model to answer with.

# Design

```
CHAT_MODELS='{"default":{"entry":"default","model":"claude-sonnet-4-6"},"options":[{"entry":"default","model":"claude-sonnet-4-6"},{"entry":"default","model":"claude-opus-4-7"}]}'
```

- **The list comes from the spec.** astro-server sets `CHAT_MODELS` from the live entries of `astropods.yml`, with the model chosen at deploy as the default. `ParseChatModels` refuses a value with no options, an option without an entry or a model, or a default that is not an option. A refused value logs a warning and leaves model switching off, so the chat keeps working.
- **`GET /api/agent/config` reports `capabilities.models`** as `{default, options}` when the list is set, the way it reports `files` and `edit`.
- **A send names its model.** `POST /api/conversations/{id}/messages` takes an optional `model: {entry, model}`. A model that is not in the list, or any model when the list is unset, returns 400 `invalid_model` before the agent runs.
- **Every web turn carries a model when the list is set.** A send without `model`, a "write your own reply" turn, and an audio turn get the default. The agent then needs no deploy-time fallback for those paths.
- **The agent receives `Message.model`**, a new `ModelChoice {entry, model}`. The entry is part of the choice so that an agent with several model entries knows which provider client to use.

The gateway key reaches every model the gateway serves. The check here keeps a chat user on the builder's list. It does not limit the agent's own code.

# Migration

None for running deployments: without `CHAT_MODELS` nothing changes, and a send without `model` behaves as before.

The SDKs move to `0.4.0`: `@astropods/messaging` and `astropods-messaging` carry `Message.model` and `ModelChoice`. The packaged adapters in `astropods/adapters` hand the model to the agent's framework.
