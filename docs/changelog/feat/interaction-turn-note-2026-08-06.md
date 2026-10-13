# Interaction turn lifecycle + resolved-interaction rows

## Summary

Completes the sidecar side of an in-chat interaction: while a blocking
interaction waits on the user the turn pauses without being reaped, and when the
user answers, the resolution is written into the thread. A submit/decline lands a
ghost `note` (a non-assistant row recording what was answered); a "write your own
reply" lands the user's prose as a `user` message. Either way the new row splits
the agent's continuation into its own bubble, and "write your own reply" resolves
as a fresh turn rather than a mid-turn injection.

## Design

**Awaiting phase.** The turn tracker gains a `phaseAwaiting` state. When the
agent emits a Renderable the turn enters it: the idle watchdog is suspended (a
user pondering a form is legitimately blocked, not hung) and the reply streamed
so far is flushed to the chat store as its own row, so a reload shows the full
preamble rather than a throttle-lagged partial. The user's answer moves the turn
back to streaming and re-arms the watchdog.

**The boundary row.** Resolving an interaction persists a non-assistant row, so
the continuation's progressive persist appends a fresh assistant row instead of
updating the pre-interaction reply — the boundary is structural, needing no flag.
A submit/decline persists a `note` carrying a compact record — a tool-permission
ask reads as `Approved`/`Denied`, a submitted form as its field values
(`Answered · Date: … · Attendees: …`, preferring schema titles). A "write your
own reply" persists the user's prose as a `user` row, so it renders as a user
message and is recorded as a user turn. Both reach the client over one
injected-message SSE event carrying the row's role; submit/decline also reset the
in-memory partial buffer so the continuation doesn't concatenate onto the flushed
preamble.

**Write your own reply.** A `RESPOND` answer is a new turn: the prose is queued on
the turn and the agent's current turn is cancelled; when that turn reaches any
terminal state (END, error, idle reap, or disconnect) a single delivery point
records the prose as a user message and forwards it as a fresh turn, so the two
never overlap and the reply is never dropped. If no turn is live at answer time it
is forwarded immediately. A failed forward surfaces an SSE error rather than
hanging the stream, and a row that fails to persist (conversation at the message
cap) is not broadcast, keeping the live view and a reload consistent.

## Migration

None. An agent that never emits a Renderable is unaffected; the `note` role and
the injected-message SSE event are additive.
