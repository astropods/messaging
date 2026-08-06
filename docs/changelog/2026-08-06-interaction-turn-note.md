# Interaction turn lifecycle + resolved-interaction notes

## Summary

Completes the sidecar side of an in-chat interaction: while a blocking
interaction waits on the user the turn pauses without being reaped, and when the
user answers, the resolution is written into the thread as a ghost note — a
non-assistant row that both records what the user answered and splits the agent's
continuation into its own message bubble. "Write your own reply" resolves as a
fresh turn rather than a mid-turn injection.

## Design

**Awaiting phase.** The turn tracker gains a `phaseAwaiting` state. When the
agent emits a Renderable the turn enters it: the idle watchdog is suspended (a
user pondering a form is legitimately blocked, not hung) and the reply streamed
so far is flushed to the chat store as its own row, so a reload shows the full
preamble rather than a throttle-lagged partial. The user's answer moves the turn
back to streaming and re-arms the watchdog.

**The note as boundary.** Resolving an interaction persists a `note` row: a
non-assistant, non-user message. It carries a compact record of the answer — a
tool-permission ask reads as `Approved`/`Denied`, a submitted form as its field
values (`Answered · Date: … · Attendees: …`, preferring schema titles), a
free-text reply as the prose. Because the trailing store row is now a non-assistant
note, the continuation's progressive persist appends a fresh assistant row instead
of updating the pre-interaction reply — the boundary is structural, needing no
separate flag. Submit/decline also reset the in-memory partial buffer so the
continuation doesn't concatenate onto the flushed preamble.

**Write your own reply.** A `RESPOND` answer is a new message, not an in-turn
answer: the prose is queued on the turn and the agent's current turn is cancelled.
When that turn finalizes, the agent-response END handler injects the note and
forwards the prose as a fresh turn, so the two never overlap. The idle watchdog is
deliberately left suspended across the cancel so the queued prose can't be dropped
by a reap in that window, and a failed forward surfaces an SSE error rather than
leaving the client's stream hung. A note that fails to persist (conversation at
the message cap) is not broadcast, keeping the live view and a reload consistent.

## Migration

None. An agent that never emits a Renderable is unaffected; the note role is
additive to the chat store and the SSE stream.
