# Interaction sidecar handling: capability gate, response endpoint, validator

## Summary

The wire types for the Renderable primitive landed earlier (`renderable` on
`AgentResponse`, `renderable_response` on `PlatformFeedback`). This adds the
sidecar side of the loop: the web adapter now handles a `renderable` payload,
persists it behind a store seam, emits it to the browser as an SSE event, and
accepts the user's answer on a response endpoint that validates and delivers it
back to the agent. It ships dark. A new `SupportsDeclarativeForms` capability is
defaulted off, so a Renderable that reaches web degrades instead of rendering,
and no interaction event is emitted in normal flow.

## Design

**Capability gate.** `AdapterCapabilities` gains `SupportsDeclarativeForms` (and a
reserved `SupportsEmbeddedComponents`, false in v1). `WebCapabilities()` and
`SlackCapabilities()` return it false. The web adapter carries an override
(`WithDeclarativeForms`) so tests and the eventual switch can turn it on without
touching the shared default.

**Handling and degradation.** `HandleAgentResponse` grows a `renderable` branch:

- Capability on: parse and re-embed the data schema (rejecting malformed JSON),
  persist the interaction, and emit an `interaction` SSE event carrying the schema
  as an object with lowercased `kind`/`actions`.
- Capability off: apply the failure contract without touching the store. A strict
  ask (no `RESPOND`) returns a typed `UNSUPPORTED` renderable response so the
  agent's `render()` rejects; a free-text-tolerant ask (`RESPOND` allowed) renders
  the prompt as ordinary streamed content, and the user's next message resolves it
  as `RESPOND{text}` rather than starting a new turn.

**Response endpoint.** `POST /api/chat/conversations/{id}/interactions/{interactionId}`
authorizes the responder (only the conversation's user; fails closed when the
owner is unknown), validates a `SUBMIT` payload against the stored schema with a
JSON Schema 2020-12 validator (`santhosh-tekuri/jsonschema/v6`), records the
response exactly once (idempotent by interaction id, race-safe), and forwards it to
the local agent as a `renderable_response` over the existing feedback channel.
Invalid content is 422, a wrong responder is 403, and a re-POST replays the
recorded outcome without re-delivering.

**Untrusted schemas.** The data schema is authored by the agent (untrusted code),
so the validator runs with external `$ref` resolution disabled — otherwise the
library's default file loader would resolve a `file://` `$ref` against the sidecar
filesystem. Schemas that don't compile are rejected at emit, so a hostile or
malformed schema never lands in the store or reaches the client.

Renderable delivery to the agent is best effort: a dropped send (no agent stream)
leaves the response recorded but the agent's `render()` unresolved until reconnect
redelivery lands with recovery. The capability-off degrade path is transient
(process-local, one pending free-text ask per conversation, owner-gated) and
intentionally forgoes the store's durability and exactly-once guarantees.

**Store seam.** An `InteractionStore` interface (append, get, record, list,
pending) decouples handling from persistence. This change ships the in-memory
implementation; the SQLite-backed one against the sidecar chat store lands later
and shares the message seq space. While the capability is off the persist path is
never hit in normal flow, so the in-memory store is exercised only by the endpoint
and tests.

The astro-server chat proxy needs no change: it forwards arbitrary subpaths and
streams SSE transparently, so the new subroute and the `interaction` event pass
through untouched.

## Migration

None. The capability is off, so no interaction event is emitted and no behavior
changes for existing deployments. Turning it on is a later, coordinated switch.

Note: validator parity with the client-side JS validator (including the
`additionalProperties` default from the schema profile) is tracked as a shared
conformance-fixture task, not yet enforced here.
