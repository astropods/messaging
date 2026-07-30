package web

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/astropods/messaging/internal/adapter"
	"github.com/astropods/messaging/internal/store/sqlite"
)

// When the agent stream ends mid-turn, HandleAgentDisconnect must finalize the
// in-flight turn: broadcast a terminal error to SSE clients and flip the
// conversation out of assistant_streaming so it doesn't hang forever.
func TestWebAdapter_HandleAgentDisconnect_FinalizesInFlightTurn(t *testing.T) {
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "chat.db"))
	if err != nil {
		t.Fatalf("open sqlite store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	a := &WebAdapter{
		connManager: NewConnectionManager(30 * time.Second),
		turns:       newTurnTracker(),
		chatStore:   st,
	}

	const conv, user = "conv-1", "user-1"
	ctx := context.Background()
	if _, err := st.EnsureForSend(ctx, conv, user, "t"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if _, err := st.AppendMessage(ctx, conv, user, "user", "hi", ""); err != nil {
		t.Fatalf("append user: %v", err)
	}
	// Turn is in flight from the agent's perspective.
	a.turns.startTurn(conv)

	// A connected SSE client that should receive the terminal error.
	conn := &SSEConnection{
		ID:             "c",
		ConversationID: conv,
		EventChan:      make(chan SSEEvent, 10),
		Done:           make(chan struct{}),
		CreatedAt:      time.Now(),
	}
	a.connManager.Add(conn)

	// The stream serving conv-1 dropped; scope the reap to that conversation.
	a.HandleAgentDisconnect(ctx, conv)

	select {
	case ev := <-conn.EventChan:
		if ev.Event != EventError {
			t.Fatalf("broadcast event = %q, want %q", ev.Event, EventError)
		}
	case <-time.After(time.Second):
		t.Fatal("no terminal event broadcast on disconnect")
	}

	got, err := st.Get(ctx, conv)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.AssistantStreaming {
		t.Fatal("conversation still derives assistant_streaming after disconnect")
	}
	if a.turns.isStreaming(conv) {
		t.Fatal("turn tracker still reports the turn in flight after disconnect")
	}
}

// A per-conversation stream's disconnect must reap only its own conversation and
// leave turns owned by other, still-live streams in flight. The shared agent
// stream (adapter.AgentStreamID) owns every in-flight turn, so it reaps them all.
func TestWebAdapter_HandleAgentDisconnect_ScopedToStream(t *testing.T) {
	a := &WebAdapter{turns: newTurnTracker()}
	ctx := context.Background()

	a.turns.startTurn("conv-A")
	a.turns.startTurn("conv-B")

	a.HandleAgentDisconnect(ctx, "conv-A")
	if a.turns.isStreaming("conv-A") {
		t.Fatal("conv-A's own stream disconnect should reap conv-A")
	}
	if !a.turns.isStreaming("conv-B") {
		t.Fatal("conv-A's disconnect wrongly reaped conv-B, owned by another live stream")
	}

	a.HandleAgentDisconnect(ctx, adapter.AgentStreamID)
	if a.turns.isStreaming("conv-B") {
		t.Fatal("shared-stream disconnect should reap every in-flight turn")
	}
}
