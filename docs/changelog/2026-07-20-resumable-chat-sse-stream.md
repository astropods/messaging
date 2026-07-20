# Summary

Makes the chat SSE stream resumable so a dropped connection is a performance
detail, not a correctness bug. Previously the hub fanned each event out only to
the connections present at broadcast time and buffered nothing, and every
subscribe began a fresh stream. So any client that (re)subscribed after a turn's
`finish` — an `EventSource` reconnect during a long turn, or the send→subscribe
gap on a fast reply — never learned the turn ended and hung on the loading
avatar until a 3-minute watchdog ("stuck avatar; reply is in the trace; refresh
shows it").

# Design

- **Monotonic event ids + per-conversation resume buffer.** Every broadcast
  event is tagged with a per-conversation sequence number (emitted as the SSE
  `id:`) and retained in a bounded, oldest-first ring. The ring is **segmented by
  turn** — a terminal event closes the segment and the next turn's first event
  starts a fresh one — so a resume only ever replays within the live turn. A
  cursor from before the current turn (the client crossed a boundary while away)
  is released with a terminal finish and reconciles from history, rather than
  being replayed a foreign turn's deltas or left hanging. It is bounded by both
  event count and
  **payload bytes** (so one outsized turn can't pin memory), plus an LRU cap on
  total conversations, and is released on stop. The buffer is independent of
  connection lifetime, so an event survives a window with zero connections.
  Buffering and fan-out happen under one lock together with register+snapshot, so
  an event is never both replayed and delivered live.

- **Honor `Last-Event-ID` on resubscribe.** A browser `EventSource` replays its
  last-seen id on reconnect; the stream replays exactly the events with a higher
  sequence (missed chunks and the terminal `finish`) before resuming live. The
  astro-server messaging proxy forwards the `Last-Event-ID` header (it previously
  dropped it). A cursor that falls outside the live segment — before it (a crossed
  turn boundary) or beyond the max (a stale id from before an eviction+recreate
  reset the sequence to 1) — is released with a finish and reconciles from history,
  since the retained ring is a different turn whose deltas can't be spliced onto the
  client's reconstruction.

- **Settle instead of guess on a fresh subscribe.** A subscribe with no cursor
  can't resume from a position, and the store snapshot alone is ambiguous: "the
  latest persisted message is the assistant's" means both "the turn finished" and
  "a new turn is starting whose `POST /messages` hasn't persisted its user row
  yet," and a just-finished turn may have broadcast its `finish` to zero
  connections while its streaming flag still lingers. Rather than decide
  synchronously — which misfired in two opposite windows (a spurious `finish` on
  a follow-up turn that raced ahead of its user-row write; a missed `finish`
  while the ended turn's streaming flag lingered) — the already-registered
  connection observes what happens next for a bounded window: a live chunk means
  the turn is live (stream it), a live finish/error ends it, and only silence
  falls back to a store-derived terminal replay, by which point any concurrent
  send has persisted and any just-ended turn has cleared its flag. A fresh
  subscribe still never replays buffered deltas (the client reconstructs by
  appending, so re-sending would double them) — resumption stays opt-in via the
  cursor.

- **Release on stop.** A client "stop generating" is terminal, so it drops the
  conversation's resume buffer instead of holding its delta ring resident until
  LRU eviction; a late reconnect falls back to the store-derived terminal replay.

# Limitations

The resume buffer is in-memory and bounded per conversation (512 events / 256 KiB)
and in total (2048 conversations, LRU-evicted). Its pathological ceiling —
every buffered conversation simultaneously holding a full-size segment — is
~512 MiB of payload (somewhat higher counting per-event `id:`/`event:` strings and
slice headers, which the byte cap does not track). Steady-state residency tracks
actual turn sizes and is far lower, but the caps should be sized against the
sidecar's memory request; lower `maxBufferedConversations` if that ceiling is a
meaningful fraction of the limit.

# Migration

None. No config, API, or schema changes; the SSE wire format is unchanged apart
from now always carrying an `id:` field. Pairs with the astro monorepo change
that forwards `Last-Event-ID` through the messaging proxy.
