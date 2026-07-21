# Summary

Adds a `files` capability to `GET /api/agent/config` so the web client can hide
the chat composer's upload affordance for agents that don't handle attachments —
instead of showing an upload button whose files the agent silently ignores.

File *storage* (`FILES_DIR`) is wired automatically for every messaging
deployment (it hangs off the shared agent volume), so "does the sidecar have
storage" is always true and can't gate the button. The signal that actually
matters is whether the *agent* consumes attachments, which only the agent knows.

# Design

- **Agent declares `supports_files`.** New field on the `AgentConfig` message the
  agent already sends over its gRPC stream at startup (`config.proto`). Opt-in: an
  unset value (older agents / agents that never wire up files) reads as false.
- **`capabilities.files` = storage AND declared.** `GET /api/agent/config` reports
  `fileStore != nil && agentConfig.supports_files`. Storage alone isn't enough (the
  agent would ignore uploads); a declaration without storage isn't either (nowhere
  to put them). The response reuses the config endpoint the client already fetches
  and astro-server already proxies — no new endpoint, no agent round-trip.
- **SDK.** `@astropods/messaging`'s `AgentConfig` gains `supportsFiles?: boolean`;
  proto-loader serializes it onto the existing wire message.
- **Client gate (paired astro change).** The composer hides its upload affordance
  unless `capabilities.files` is true, defaulting to hidden when the field is
  absent. It ships *after* this sidecar change so a hides-unless-enabled client
  never runs against a sidecar that doesn't yet report the field.

The earlier storage-only `AdapterCapabilities.SupportsFiles` capability is removed
— it had no consumers and encoded the wrong signal.

# Migration

Opt-in: an agent that consumes file uploads must set `supportsFiles: true` in the
config it declares (via the SDK / adapter-core) and redeploy, otherwise the web
client hides the upload button. Agents that don't handle files need no change.
Additive field on the `agent/config` response; older clients ignore it.

Shipping this requires publishing both SDK artifacts, not just the sidecar image:
an agent can only declare `supports_files` if its bundled SDK proto carries the
field. The TypeScript (`@astropods/messaging`) and Python (`astropods-messaging`)
packages are both bumped to `0.1.2` and their generated stubs regenerated from the
updated `config.proto`; publish both (npm + PyPI) alongside the sidecar image.
Existing agents pick up the field only on a fresh build/redeploy against the new SDK.
