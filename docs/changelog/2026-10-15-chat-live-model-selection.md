# Summary

An agent answered every web-chat turn with one model, and a chat user could not switch. An agent can now declare the models a chat user may pick from when it registers. The web chat shows them in a model picker, and this sidecar checks each pick and tells the agent which model to answer with.

# Design

- **The agent declares the list.** `AgentConfig.models` is a list of model ids, the first being the agent's default. The ids mean whatever the agent's code makes of them.
- **`GET /api/agent/config` reports `capabilities.models`** with the agent's list, the way it reports `files` and `edit`. It is absent when the agent declares none.
- **A send names its model.** `POST /api/conversations/{id}/messages` takes an optional `model`. A model the agent does not offer, or any model when it offers none or has not registered yet, returns 400 `invalid_model` before the agent runs.
- **The agent receives `Message.model`**, the picked id. A send without `model`, a "write your own reply" turn, and an audio turn carry an empty value, which leaves the agent on its default.

The platform's AI Gateway key reaches every model the gateway serves. This check keeps a chat user on the agent's list; it does not limit the agent's own code.

# Migration

None for running deployments: an agent that declares no models behaves as before, and a send without `model` is unchanged.

The SDKs move to `0.4.0`: `@astropods/messaging` and `astropods-messaging` carry `Message.model` and `AgentConfig.models`. The packaged adapters in `astropods/adapters` take a `models` option and pass the pick to the agent's framework.
