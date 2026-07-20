package web

import (
	"strings"
	"testing"
	"time"

	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
)

// These tests cover the two opposite ways the synchronous terminal-state guess
// on a fresh subscribe misfired, both closed by settleFreshSubscribe: rather
// than deciding from a store snapshot that is ambiguous mid-race, the handler
// observes what actually happens for a bounded window before falling back to the
// store.

// settleWindow is small so the fallback timer fires well inside runStream's
// post-subscribe window, but comfortably larger than the in-test store writes /
// in-memory mutations the tests perform during the settle.
const settleWindow = 25 * time.Millisecond

// Start-of-turn race (the false positive): a follow-up turn's fresh subscribe
// can reach the sidecar before POST /messages persists the new user row, so the
// latest persisted message is still the *previous* turn's assistant reply. The
// old synchronous fallback read that as "already finished" and replayed a spurious
// finish, killing the new turn's stream. The settle waits out the send: once the
// user row lands, the recheck sees an in-flight turn and no finish is sent.
func TestHandleStream_StartRace_NoSpuriousFinishWhenUserRowLandsDuringSettle(t *testing.T) {
	h, st := streamHandlersWithStore(t)
	h.freshSubscribeSettle = settleWindow
	const (
		convID = "conv-followup"
		user   = "user-1"
	)
	ctx := t.Context()
	if err := st.Upsert(ctx, convID, user, "chat"); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	// A prior turn completed: latest persisted row is the assistant's reply.
	if _, err := st.AppendMessage(ctx, convID, user, "user", "hi", ""); err != nil {
		t.Fatalf("seed user message: %v", err)
	}
	if _, err := st.AppendMessage(ctx, convID, user, "assistant", "old reply", ""); err != nil {
		t.Fatalf("seed assistant reply: %v", err)
	}

	// The new turn's user row lands during the settle — simulating POST /messages
	// persisting just after the stream subscribed.
	body := runStream(t, h, convID, user, func() {
		if _, err := st.AppendMessage(ctx, convID, user, "user", "follow up", ""); err != nil {
			t.Errorf("persist follow-up user message: %v", err)
		}
	})

	if strings.Contains(body, "event: "+EventFinish) {
		t.Fatalf("a follow-up turn racing its user-row write must not be finished early, got:\n%s", body)
	}
}

// End-of-turn race (the false negative): on END the sidecar broadcasts the finish
// (possibly to zero connections), persists the reply, then clears the turn's
// streaming flag. A fresh subscribe landing after the broadcast but before the
// clear saw isStreaming==true, so the old fallback suppressed the terminal replay
// and the client hung until the watchdog. The settle waits past the clear, then
// the recheck sees a terminal turn and replays the finish.
func TestHandleStream_EndRace_ReplaysFinishAfterStreamingClearsDuringSettle(t *testing.T) {
	h, st := streamHandlersWithStore(t)
	h.freshSubscribeSettle = settleWindow
	h.turns = newTurnTracker()
	const (
		convID = "conv-end-race"
		user   = "user-1"
	)
	ctx := t.Context()
	if err := st.Upsert(ctx, convID, user, "chat"); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	if _, err := st.AppendMessage(ctx, convID, user, "user", "hi", ""); err != nil {
		t.Fatalf("seed user message: %v", err)
	}
	// The reply is persisted (latest row is the assistant's)…
	if _, err := st.AppendMessage(ctx, convID, user, "assistant", "the reply", ""); err != nil {
		t.Fatalf("seed assistant reply: %v", err)
	}
	// …but the turn's streaming flag still lingers (clear() hasn't run yet), so a
	// synchronous check would wrongly treat the turn as live.
	h.turns.record(convID, &pb.ContentChunk{Type: pb.ContentChunk_START, Content: "the reply"})

	// clear() runs during the settle (the persist finished, tracker released).
	body := runStream(t, h, convID, user, func() {
		h.turns.clear(convID)
	})

	if !strings.Contains(body, "event: "+EventFinish) {
		t.Fatalf("a finished turn whose streaming flag cleared during the settle must be replayed a finish, got:\n%s", body)
	}
}

// A live event during the settle wins over the store: even when the store still
// shows the previous turn as terminal, a new turn's first chunk arriving on the
// wire means the turn is live — deliver it and hand off to the event loop rather
// than synthesizing a finish.
func TestHandleStream_LiveChunkDuringSettleIsDeliveredNotPreempted(t *testing.T) {
	h, st := streamHandlersWithStore(t)
	h.freshSubscribeSettle = settleWindow
	const (
		convID = "conv-live-chunk"
		user   = "user-1"
	)
	ctx := t.Context()
	if err := st.Upsert(ctx, convID, user, "chat"); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	if _, err := st.AppendMessage(ctx, convID, user, "user", "hi", ""); err != nil {
		t.Fatalf("seed user message: %v", err)
	}
	if _, err := st.AppendMessage(ctx, convID, user, "assistant", "old reply", ""); err != nil {
		t.Fatalf("seed assistant reply: %v", err)
	}

	body := runStream(t, h, convID, user, func() {
		h.connManager.Broadcast(convID, SSEEvent{Event: EventChunk, Data: `{"type":"chunk","content":"new turn"}`})
	})

	if strings.Contains(body, "event: "+EventFinish) {
		t.Fatalf("a live chunk during the settle means the turn is live; no finish should be synthesized, got:\n%s", body)
	}
	if !strings.Contains(body, "new turn") {
		t.Fatalf("the live chunk should be delivered, got:\n%s", body)
	}
}

// Eviction hardening: after maxBufferedConversations evicts a buffer, the next
// broadcast recreates it with the sequence restarting at 1. A client reconnecting
// with a cursor from before the eviction carries a higher id than the reset
// buffer's max, which would make a naive seq comparison match nothing and suppress
// the replay — re-stranding the UI. Such a cursor replays the whole retained ring.
func TestAddWithResume_ReplaysWholeRingWhenCursorAheadOfBuffer(t *testing.T) {
	cm := NewConnectionManager(time.Hour)
	const convID = "conv-reset"

	// A recreated buffer's sequence starts at 1.
	cm.Broadcast(convID, SSEEvent{Event: EventChunk, Data: `{"type":"chunk","content":"a"}`}) // seq 1
	cm.Broadcast(convID, NewFinishEvent(""))                                                  // seq 2

	conn := &SSEConnection{
		ID:             "c1",
		ConversationID: convID,
		EventChan:      make(chan SSEEvent, 8),
		Done:           make(chan struct{}),
	}
	// Cursor 50 is far beyond the reset buffer's max (2) — a pre-eviction id.
	missed, caughtUp := cm.AddWithResume(conn, 50)

	if len(missed) != 2 {
		t.Fatalf("a cursor ahead of the buffer must replay the whole ring (2 events), got %d", len(missed))
	}
	if caughtUp {
		t.Fatalf("a stale pre-eviction cursor must not be treated as caught up")
	}
	var sawFinish bool
	for _, e := range missed {
		if e.Event == EventFinish {
			sawFinish = true
		}
	}
	if !sawFinish {
		t.Fatalf("replay must include the terminal finish so the client is released, got %+v", missed)
	}
}

// Turn segmentation: the buffer isn't shared across turns. A new turn's first
// event opens a fresh segment, so a resume with a stale cursor from an earlier
// turn replays only the current turn — never a prior finished turn's deltas or
// its finish (which would double-render or prematurely close the live turn).
func TestBuffer_ResumeReplaysOnlyCurrentTurn(t *testing.T) {
	cm := NewConnectionManager(time.Hour)
	const convID = "conv-multi-turn"

	cm.Broadcast(convID, SSEEvent{Event: EventChunk, Data: `{"type":"chunk","content":"turn-1"}`}) // seq 1
	cm.Broadcast(convID, NewFinishEvent(""))                                                       // seq 2 (turn 1 ends)
	cm.Broadcast(convID, SSEEvent{Event: EventChunk, Data: `{"type":"chunk","content":"turn-2"}`}) // seq 3 (turn 2)

	conn := &SSEConnection{
		ID:             "c1",
		ConversationID: convID,
		EventChan:      make(chan SSEEvent, 8),
		Done:           make(chan struct{}),
	}
	missed, _ := cm.AddWithResume(conn, 1) // stale cursor from mid-turn-1

	var sawTurn2 bool
	for _, e := range missed {
		if e.Event == EventFinish {
			t.Fatalf("resume must not replay a previous turn's finish")
		}
		if strings.Contains(e.Data, "turn-1") {
			t.Fatalf("resume must not replay a previous turn's deltas")
		}
		if strings.Contains(e.Data, "turn-2") {
			sawTurn2 = true
		}
	}
	if !sawTurn2 {
		t.Fatalf("resume should replay the current turn's delta, got %d events", len(missed))
	}
}

// The buffer is bounded by payload bytes, not just event count, so one
// conversation with an outsized turn can't pin unbounded memory.
func TestBuffer_ByteCapDropsOldest(t *testing.T) {
	cm := NewConnectionManager(time.Hour)
	const convID = "conv-bytes"

	big := strings.Repeat("x", 100*1024) // 100 KiB each; 4 > the 256 KiB cap
	for i := 0; i < 4; i++ {
		cm.Broadcast(convID, SSEEvent{Event: EventChunk, Data: big})
	}

	buf := cm.eventBuffers[convID]
	if buf == nil {
		t.Fatalf("expected a resume buffer")
	}
	if buf.bytes > maxBufferedBytesPerConv {
		t.Fatalf("retained bytes %d exceed the cap %d", buf.bytes, maxBufferedBytesPerConv)
	}
	if len(buf.events) >= 4 {
		t.Fatalf("byte cap should have dropped oldest events, still holding %d", len(buf.events))
	}
}

// A reconnect whose cursor already covers the latest event (it saw the finish)
// must not be sent a synthesized duplicate finish.
func TestHandleStream_CaughtUpResumeDoesNotDuplicateFinish(t *testing.T) {
	h, st := streamHandlersWithStore(t)
	h.freshSubscribeSettle = settleWindow
	const (
		convID = "conv-dup-finish"
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
	h.connManager.Broadcast(convID, NewFinishEvent("")) // seq 1 — the finish the client saw

	// Reconnect with Last-Event-ID == the finish's id (client is caught up).
	body := runStreamResume(t, h, convID, user, "1", nil)

	if strings.Contains(body, "event: "+EventFinish) {
		t.Fatalf("a caught-up reconnect must not be sent a duplicate finish, got:\n%s", body)
	}
}

// A client stop is terminal: CloseConversation drops the resume buffer so its
// delta ring isn't held resident until LRU eviction.
func TestCloseConversation_ReleasesResumeBuffer(t *testing.T) {
	cm := NewConnectionManager(time.Hour)
	const convID = "conv-stopped"

	cm.Broadcast(convID, SSEEvent{Event: EventChunk, Data: `{"type":"chunk","content":"x"}`})
	if cm.eventBuffers[convID] == nil {
		t.Fatalf("precondition: a broadcast should create a resume buffer")
	}

	cm.CloseConversation(convID)

	if cm.eventBuffers[convID] != nil {
		t.Fatalf("a stop must release the conversation's resume buffer")
	}
}
