# Interaction reload support + declarative-forms capability lever

## Summary

Two sidecar additions that complete the web-facing side of the interaction loop.
The conversation fetch now surfaces still-open interactions so a reloaded (or
reconnecting) client re-renders its pending forms, and the web adapter gains an
env lever to turn the declarative-forms capability on — the switch that moves the
feature from degrade to render. Both are inert by default.

## Design

**Pending interactions on the conversation fetch.** `GET /api/chat/conversations/{id}`
now returns `pending_interactions`: the conversation's still-pending blocking
interactions (the FIFO queue) in the same client shape as the `interaction` SSE
event. The SSE event builder was refactored so the event and the fetch share one
`interactionEventData` mapping (schema/value re-embedded as JSON objects, actions
lowercased); a malformed stored Renderable is skipped rather than failing the
fetch. Empty when no interaction store is wired.

This is what lets the client re-enter `waiting-for-input` after a reload: the SSE
event is the live fast path, and the persisted pending queue is the durable source
on reconnect.

**Declarative-forms capability lever.** The web adapter's `SupportsDeclarativeForms`
capability (defaulted off) is now flippable via `WEB_DECLARATIVE_FORMS=true`, wired
in `cmd/server/main.go`. While off, Renderables degrade per the failure contract;
on, they render. This is the staged-rollout switch — off in every environment until
turned on deliberately.

## Migration

None. `pending_interactions` is additive, and the capability stays off unless
`WEB_DECLARATIVE_FORMS=true` is set.
