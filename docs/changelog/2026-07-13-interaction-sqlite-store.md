# Interaction persistence: SQLite-backed InteractionStore

## Summary

Phase 3 shipped interaction handling behind an in-memory `InteractionStore` seam so
it could land without depending on the SQLite chat store. That store has since
merged, so this backs the seam with durable SQLite: a Renderable and its response
now survive a page reload and a pod reschedule, and interactions take their place
in the conversation's ordering alongside messages. The interaction capability is
still off, so this is dark — the durable store is exercised by tests and is ready
for the web client to render against.

## Design

**Interactions table.** A third table beside `conversations` and `messages`, keyed
`(conversation_id, id)` with the Renderable stored as `render_json` (protojson) and
a `status` lifecycle (`pending` → `submitted`/`declined`/`cancelled`/`responded`).
A partial index on pending rows backs the blocking-queue read.

**Shared sequence.** Interactions interleave with messages by a single monotonic
`seq`. Seq allocation moved to one `nextSeqTx` helper that maxes over both tables,
used by message-append and interaction-append alike, so the combined timeline is
strictly ordered with no cross-table collision. While the capability is off the
interactions table is empty in normal flow, so this is behavior-preserving for
existing message writes.

**Store semantics** mirror the in-memory implementation the endpoint already relied
on: append is idempotent by id (a re-emitted Renderable returns the existing row,
never resetting an answered one), and record-response is race-safe and idempotent
(the read-then-update runs in one transaction over the single connection, so a
concurrent second responder gets the stored outcome rather than a double delivery).

**Soft-delete cancels pending interactions.** Deleting a conversation now, in the
same transaction, flips its pending interactions to `cancelled` and returns their
ids; the delete handler delivers a `CANCEL` response to the agent for each, so a
suspended turn resolves instead of hanging.

**Interface is now context-aware.** `InteractionStore` methods take a `context.Context`,
matching the rest of the chat store, so the SQLite implementation honors request
cancellation. The in-memory implementation stays for tests.

The SQLite store is wired into the web adapter in place of the in-memory default
whenever chat persistence is configured.

## Migration

None. The interaction capability is off, so no interactions are created in normal
flow and no behavior changes for existing deployments. The new table is created on
schema init and stays empty until the feature switches on.
