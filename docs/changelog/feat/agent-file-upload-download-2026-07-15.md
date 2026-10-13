# Agent file upload/download + per-user isolation + storage capacity

## Summary

Wires per-message file attachments through the web ("platform") chat end to end,
scopes every file to the user who created it, and adds a storage-capacity signal
so the volume can't silently fill. Builds on the chat-ownership/reliability
hardening also on this branch (fail-closed ownership on chat reads/writes/cancel,
stuck-turn finalization, SQL-paginated thread reads).

## Design

**Attachments contract.** `Attachment.storage_key` (new proto field) carries the
opaque files-API key on the message forwarded to the agent; the sidecar resolves
the on-disk `<key>.blob` under `FILES_DIR`. On send, `POST …/messages` accepts
`attachments: [{key}]`; the handler re-reads authoritative metadata from the file
store and rejects unknown keys, keys owned by another user, and sends over a fixed
per-message cap. Agent replies' `ResponseAttachment`s (previously web-ignored) are
now consumed, persisted, and re-emitted as download chips.

**Files store.** `FileStore` (filesystem `FSStore` today, presign-ready for S3)
gains `Usage()` — `statfs` on the backing volume. `List` dedupes an agent-written
plain file against a managed upload of the same name (managed record wins).

**Per-user isolation.** Files carry an `UploadedBy` owner. Every files-API
operation (list/get/content/put/delete) and every attachment reference is checked
via `ownsFile(session, meta)`, returning "not found" for another user's file so
existence never leaks across users of the same account. Agent-produced files are
attributed to the conversation owner so the reply's download chip works for them
and no one else.

**Persistence.** A `messages.attachments` JSON column round-trips attachments
through history and the SSE stream. There is no migration framework, so the column
is added with a guarded `ALTER TABLE` (PRAGMA `table_info` check) on open;
`UpsertAssistantProgress` only writes attachments when non-empty so it can't clobber
a persisted set with an empty one.

**Storage capacity.** `GET /api/files/usage` reports `{total,used,available}` from
`statfs` — the whole shared volume (chat DB + files + agent outputs), the real
capacity uploads compete for. `HandleCreateFile` rejects with **507 Insufficient
Storage** when the declared size plus a 32 MiB reserve won't fit, before any bytes
upload; a mid-write `ENOSPC` is also mapped to 507. Works identically in cluster
and `ast dev` (no metrics backend needed).

## Migration

The `messages.attachments` column is added automatically on existing volumes; no
operator action. `Attachment.storage_key` is additive. Regenerating the Python SDK
proto is recommended so Python agents receive the resolved attachment `path`
(without it they fall back to scanning `AGENT_FILES_DIR`, which still works).
