package web

import (
	"strings"
	"testing"
)

// Resumable-stream tests. The sidecar tags every broadcast event with a
// per-conversation monotonic id and retains a bounded buffer, so a client that
// reconnects with its Last-Event-ID replays exactly what it missed — closing the
// lost-event gap generally (missed chunks and the terminal finish), not just the
// finish-on-fresh-subscribe special case.

// The core case: a client saw event 1, dropped, and the finish (event 2) was
// broadcast to zero connections during the gap. On reconnect with Last-Event-ID
// 1, the sidecar replays event 2 — and only event 2.
func TestHandleStream_ResumesFromLastEventIDAfterGap(t *testing.T) {
	h := streamHandlers()
	const convID = "conv-resume-gap"

	// Streamed while connected (client's cursor advances to 1)…
	h.connManager.Broadcast(convID, SSEEvent{Event: EventChunk, Data: `{"type":"chunk","content":"partial"}`}) // seq 1
	// …then the connection drops and the finish lands on zero connections.
	h.connManager.Broadcast(convID, NewFinishEvent("")) // seq 2

	body := runStreamResume(t, h, convID, "", "1", nil)

	if !strings.Contains(body, "event: "+EventFinish) {
		t.Fatalf("reconnect must replay the finish missed during the gap, got:\n%s", body)
	}
	if strings.Contains(body, "partial") {
		t.Fatalf("must not replay events at or below the resume cursor (seq 1), got:\n%s", body)
	}
	if !strings.Contains(body, "id: 2") {
		t.Fatalf("replayed event should carry its monotonic id, got:\n%s", body)
	}
}

// Every broadcast event carries a monotonic per-conversation id, replayed in
// order. Resuming from 0 replays the whole retained buffer.
func TestHandleStream_AssignsMonotonicEventIDs(t *testing.T) {
	h := streamHandlers()
	const convID = "conv-ids"

	h.connManager.Broadcast(convID, SSEEvent{Event: EventChunk, Data: `{"type":"chunk","content":"a"}`}) // seq 1
	h.connManager.Broadcast(convID, SSEEvent{Event: EventChunk, Data: `{"type":"chunk","content":"b"}`}) // seq 2

	body := runStreamResume(t, h, convID, "", "0", nil)

	i1, i2 := strings.Index(body, "id: 1"), strings.Index(body, "id: 2")
	if i1 < 0 || i2 < 0 {
		t.Fatalf("events must carry monotonic ids, got:\n%s", body)
	}
	if i1 > i2 {
		t.Fatalf("replay must preserve broadcast order, got:\n%s", body)
	}
}

// A fresh subscribe (no Last-Event-ID) must NOT replay buffered deltas: the
// client accumulates chunks by appending, so re-sending ones it may already hold
// would double the reply. Resumption is opt-in via the cursor.
func TestHandleStream_FreshSubscribeDoesNotReplayBufferedDeltas(t *testing.T) {
	h := streamHandlers()
	const convID = "conv-fresh"

	h.connManager.Broadcast(convID, SSEEvent{Event: EventChunk, Data: `{"type":"chunk","content":"buffered-delta"}`}) // seq 1

	body := runStreamResume(t, h, convID, "", "", nil) // no Last-Event-ID

	if !strings.Contains(body, "event: "+EventConnected) {
		t.Fatalf("expected a connected event, got:\n%s", body)
	}
	if strings.Contains(body, "buffered-delta") {
		t.Fatalf("a fresh subscribe must not replay buffered deltas, got:\n%s", body)
	}
}

// Safety net: if the resume cursor predates the retained buffer (very long turn)
// or the buffer was evicted, the replay carries no terminal event — so a client
// resuming into an already-finished turn must still be released via the store's
// terminal state rather than hanging.
func TestHandleStream_ResumeSafetyNetWhenBufferMissesTerminal(t *testing.T) {
	h, st := streamHandlersWithStore(t)
	const (
		convID = "conv-resume-evicted"
		user   = "user-1"
	)
	ctx := t.Context()
	if err := st.Upsert(ctx, convID, user, "chat"); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	if _, err := st.AppendMessage(ctx, convID, user, "user", "hi", ""); err != nil {
		t.Fatalf("seed user message: %v", err)
	}
	if _, err := st.AppendMessage(ctx, convID, user, "assistant", "done", ""); err != nil {
		t.Fatalf("seed assistant reply: %v", err)
	}

	// Nothing buffered for this conversation; the client resumes from a stale id.
	body := runStreamResume(t, h, convID, user, "42", nil)

	if !strings.Contains(body, "event: "+EventFinish) {
		t.Fatalf("a resume whose buffer lacks the terminal event must still release a finished turn via the store, got:\n%s", body)
	}
}
