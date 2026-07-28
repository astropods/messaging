# Interaction reload support + declarative forms always on

## Summary

Two sidecar changes that finish the web-facing interaction loop. The conversation
fetch now surfaces still-open interactions so a reloaded or reconnecting client
re-renders its pending forms, and the web adapter now renders declarative forms
unconditionally — the dark-launch capability toggle and the degrade fallback it
gated are removed.

## Design

**Pending interactions on the conversation fetch.** `GET /api/chat/conversations/{id}`
returns `pending_interactions`: the conversation's still-pending blocking
interactions (the FIFO queue) in the same client shape as the `interaction` SSE
event. The event and the fetch share one `interactionEventData` mapping; a
malformed stored Renderable is skipped rather than failing the fetch. Empty when
no interaction store is wired. This is what lets a client re-enter
`waiting-for-input` after a reload — the SSE event is the live fast path, the
persisted queue is the durable source on reconnect.

**Declarative forms are always on.** The web adapter emits every Renderable as an
interaction; there is no gate beyond the agent choosing to call `render()` /
`elicit()`. Removed: the `WEB_DECLARATIVE_FORMS` env toggle, the
`WithDeclarativeForms` option and its capability override, and the whole degrade
path (strict ask → typed `UNSUPPORTED`, free-text-tolerant ask → text prompt
resolved as `RESPOND`, plus the per-conversation degrade tracker). The degrade
path only ever ran while the capability was off; with forms always rendered on
web it was unreachable, and the renderer's built-in "write your own reply" already
covers the free-text case, so nothing user-facing is lost.

## Migration

None. `pending_interactions` is additive, and an agent that never emits a
Renderable is unaffected.
