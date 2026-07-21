package web

import (
	"net/http/httptest"
	"strconv"
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

// A cancel broadcasts a terminal finish and then closes conn.Done in the same
// call. select picks a ready case at random, so a fresh subscriber still in its
// settle window can have Done win over the enqueued finish; the Done branch must
// drain EventChan first so the finish is flushed rather than dropped.
func TestSettleFreshSubscribe_DrainsFinishWhenDoneWins(t *testing.T) {
	h, _ := streamHandlersWithStore(t)
	h.freshSubscribeSettle = time.Hour // only Done/EventChan drive the return, not the timer

	// Repeat: without the drain the finish is dropped whenever Done wins (~half the
	// time), which the loop makes reliably observable.
	for i := 0; i < 200; i++ {
		conn := &SSEConnection{
			ID:             "c1",
			ConversationID: "conv-cancel",
			EventChan:      make(chan SSEEvent, 4),
			Done:           make(chan struct{}),
		}
		conn.EventChan <- NewFinishEvent("") // terminal enqueued by the cancel's broadcast
		close(conn.Done)                     // …then Done closed in the same call

		rec := httptest.NewRecorder()
		h.settleFreshSubscribe(t.Context(), rec, rec, conn, "conv-cancel")

		if !strings.Contains(rec.Body.String(), "event: "+EventFinish) {
			t.Fatalf("iter %d: settle dropped the enqueued finish when Done won the select", i)
		}
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
// A cursor beyond the buffer's max means the sequence was reset by an
// eviction+recreate, so the retained ring belongs to a different (possibly live)
// turn. It must be released as a crossed boundary, not replayed, so a foreign
// turn's deltas are never spliced onto the client's stale reconstruction.
func TestAddWithResume_CursorAheadOfBufferReleasesClient(t *testing.T) {
	cm := NewConnectionManager(time.Hour)
	const convID = "conv-reset"

	// A recreated buffer's sequence starts at 1; this turn is still live (no finish).
	cm.Broadcast(convID, SSEEvent{Event: EventChunk, Data: `{"type":"chunk","content":"foreign-a"}`}) // seq 1
	cm.Broadcast(convID, SSEEvent{Event: EventChunk, Data: `{"type":"chunk","content":"foreign-b"}`}) // seq 2

	conn := &SSEConnection{
		ID:             "c1",
		ConversationID: convID,
		EventChan:      make(chan SSEEvent, 8),
		Done:           make(chan struct{}),
	}
	// Cursor 50 is far beyond the reset buffer's max (2) — a pre-eviction id.
	missed, caughtUp, crossedBoundary := cm.AddWithResume(conn, 50)

	if !crossedBoundary {
		t.Fatalf("a cursor beyond the reset buffer must flag a crossed boundary, not replay a foreign turn")
	}
	if len(missed) != 0 {
		t.Fatalf("a boundary release must replay nothing, got %d events", len(missed))
	}
	if caughtUp {
		t.Fatalf("a stale pre-eviction cursor must not be treated as caught up")
	}
}

// A long single turn can overflow the per-conversation cap and evict its own early
// deltas. A resume whose cursor lands in that evicted hole must be released to
// reconcile from history, not replayed a gapped delta stream that corrupts the
// client's appended reconstruction.
func TestAddWithResume_EvictedMidTurnHoleReleasesClient(t *testing.T) {
	cm := NewConnectionManager(time.Hour)
	const convID = "conv-long-turn"

	// One long turn that overflows the count cap, evicting its earliest deltas while
	// turnStartSeq stays pinned at 1.
	total := maxBufferedEventsPerConv + 100
	for i := 0; i < total; i++ {
		cm.Broadcast(convID, SSEEvent{Event: EventChunk, Data: `{"type":"chunk","content":"x"}`})
	}

	conn := &SSEConnection{
		ID:             "c1",
		ConversationID: convID,
		EventChan:      make(chan SSEEvent, 8),
		Done:           make(chan struct{}),
	}
	// Cursor 50 was evicted (oldest retained is ~101), so its next expected event is gone.
	missed, caughtUp, crossedBoundary := cm.AddWithResume(conn, 50)

	if !crossedBoundary {
		t.Fatalf("a cursor in an evicted mid-turn hole must flag a crossed boundary, not replay a gapped stream")
	}
	if len(missed) != 0 {
		t.Fatalf("a boundary release must replay nothing, got %d events", len(missed))
	}
	if caughtUp {
		t.Fatalf("an evicted cursor must not be treated as caught up")
	}
}

// After a buffer is LRU-evicted and later recreated, its sequence must not reset
// low enough for a stale cursor to alias the new turn's deltas. The seqFloor
// high-water mark keeps numbering monotonic across eviction so the boundary guard
// releases the stale cursor instead of splicing a foreign turn onto it.
func TestAddWithResume_SeqSurvivesEvictionSoStaleCursorReleases(t *testing.T) {
	cm := NewConnectionManager(time.Hour)
	const convID = "conv-evicted"

	// Seed the conversation and make it strictly the oldest buffer so churn evicts it.
	cm.Broadcast(convID, SSEEvent{Event: EventChunk, Data: `{"type":"chunk","content":"a"}`}) // seq 1
	cm.Broadcast(convID, SSEEvent{Event: EventChunk, Data: `{"type":"chunk","content":"b"}`}) // seq 2 (cursor lands here)
	time.Sleep(2 * time.Millisecond)

	// Churn past the conversation cap so the seeded buffer is LRU-evicted.
	for i := 0; i < maxBufferedConversations; i++ {
		cm.Broadcast("filler-"+strconv.Itoa(i), SSEEvent{Event: EventChunk, Data: `{"type":"chunk","content":"f"}`})
	}

	// A new, unrelated turn on the same conversation recreates the buffer and,
	// being long, climbs past the old cursor (2) — the aliasing window.
	for i := 0; i < 5; i++ {
		cm.Broadcast(convID, SSEEvent{Event: EventChunk, Data: `{"type":"chunk","content":"new"}`})
	}

	conn := &SSEConnection{
		ID:             "c1",
		ConversationID: convID,
		EventChan:      make(chan SSEEvent, 8),
		Done:           make(chan struct{}),
	}
	missed, _, crossedBoundary := cm.AddWithResume(conn, 2) // stale cursor from the evicted turn

	if !crossedBoundary {
		t.Fatalf("a stale cursor from an evicted+recreated buffer must flag a crossed boundary")
	}
	if len(missed) != 0 {
		t.Fatalf("a boundary release must replay nothing, got %d foreign events", len(missed))
	}
}

// A resume cursor from before the current turn (the client crossed a turn
// boundary while away) must flag a crossed boundary and replay nothing, so the
// caller releases it with a finish rather than replaying a foreign live turn.
func TestBuffer_CrossTurnCursorSignalsBoundary(t *testing.T) {
	cm := NewConnectionManager(time.Hour)
	const convID = "conv-multi-turn"

	cm.Broadcast(convID, SSEEvent{Event: EventChunk, Data: `{"type":"chunk","content":"turn-1"}`}) // seq 1
	cm.Broadcast(convID, NewFinishEvent(""))                                                       // seq 2 (turn 1 ends)
	cm.Broadcast(convID, SSEEvent{Event: EventChunk, Data: `{"type":"chunk","content":"turn-2"}`}) // seq 3 (turn 2 starts)

	conn := &SSEConnection{
		ID:             "c1",
		ConversationID: convID,
		EventChan:      make(chan SSEEvent, 8),
		Done:           make(chan struct{}),
	}
	missed, _, crossedBoundary := cm.AddWithResume(conn, 1) // stale cursor from turn 1

	if !crossedBoundary {
		t.Fatalf("a cursor before the current turn's first seq must flag a crossed boundary")
	}
	if len(missed) != 0 {
		t.Fatalf("a crossed-boundary resume must not replay the live turn's deltas, got %d", len(missed))
	}
}

// Regression for the segmentation-introduced hang: a client reconnecting with a
// cursor from a prior turn while a new turn is live must be released with a finish,
// not left hanging with the live turn's deltas appended to its stale reconstruction.
func TestHandleStream_CrossTurnResumeReleasesClient(t *testing.T) {
	h, st := streamHandlersWithStore(t)
	h.freshSubscribeSettle = settleWindow
	const (
		convID = "conv-cross-turn"
		user   = "user-1"
	)
	ctx := t.Context()
	if err := st.Upsert(ctx, convID, user, "chat"); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	// Turn 1 done; turn 2 in progress (latest row is the user's => streamTurnTerminal
	// stays false, so the old safety net would be suppressed and the client hang).
	for _, m := range []struct{ role, content string }{
		{"user", "hi1"}, {"assistant", "reply1"}, {"user", "hi2"},
	} {
		if _, err := st.AppendMessage(ctx, convID, user, m.role, m.content, ""); err != nil {
			t.Fatalf("seed %s: %v", m.role, err)
		}
	}
	// Turn 1 (chunk, finish) then turn 2's chunk — segmentation opens a new segment
	// at seq 3, so turn 1's finish is no longer retained.
	h.connManager.Broadcast(convID, SSEEvent{Event: EventChunk, Data: `{"type":"chunk","content":"turn-1"}`}) // seq 1
	h.connManager.Broadcast(convID, NewFinishEvent(""))                                                       // seq 2
	h.connManager.Broadcast(convID, SSEEvent{Event: EventChunk, Data: `{"type":"chunk","content":"turn-2"}`}) // seq 3

	body := runStreamResume(t, h, convID, user, "1", nil) // reconnect with a turn-1 cursor

	if !strings.Contains(body, "event: "+EventFinish) {
		t.Fatalf("a cross-turn reconnect must be released with a finish, got:\n%s", body)
	}
	if strings.Contains(body, "turn-2") {
		t.Fatalf("a cross-turn reconnect must not replay the foreign live turn's deltas, got:\n%s", body)
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

// A synthetic finish carries the conversation's latest id, so a reconnecting
// client advances its cursor past it instead of replaying the same stale cursor
// back into the release path (a reconnect loop).
func TestHandleStream_SyntheticFinishIsTagged(t *testing.T) {
	h, st := streamHandlersWithStore(t)
	h.freshSubscribeSettle = settleWindow
	const (
		convID = "conv-tagged"
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
	h.connManager.Broadcast(convID, NewFinishEvent("")) // seq 1 — sets the latest id

	// Fresh subscribe to the finished turn → the settle synthesizes a finish.
	body := runStream(t, h, convID, user, nil)

	if !strings.Contains(body, "event: "+EventFinish) {
		t.Fatalf("expected a synthesized finish, got:\n%s", body)
	}
	if !strings.Contains(body, "id: 1") {
		t.Fatalf("the synthetic finish must carry the latest id so the cursor advances, got:\n%s", body)
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
