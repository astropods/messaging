package web

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/astropods/messaging/internal/adapter"
	"github.com/astropods/messaging/internal/store/sqlite"
)

// A turn that goes idle past the window is finalized through the real adapter
// wiring (tracker idle timer -> reapIdleTurn -> failTurn): the SSE client gets a
// terminal error and the store stops deriving assistant_streaming.
func TestWebAdapter_IdleTimeout_FinalizesTurn(t *testing.T) {
	ctx := context.Background()
	const conv, user = "conv-idle-1", "user-1"

	st, err := sqlite.Open(filepath.Join(t.TempDir(), "chat.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.EnsureForSend(ctx, conv, user, "t"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if _, err := st.AppendMessage(ctx, conv, user, "user", "hi", ""); err != nil {
		t.Fatalf("append: %v", err)
	}

	wa := New(WithTurnIdleTimeout(60 * time.Millisecond))
	if err := wa.Initialize(ctx, adapter.Config{}); err != nil {
		t.Fatalf("init: %v", err)
	}
	wa.SetChatStore(st)

	conn := &SSEConnection{
		ID:             "c",
		ConversationID: conv,
		EventChan:      make(chan SSEEvent, 16),
		Done:           make(chan struct{}),
		CreatedAt:      time.Now(),
	}
	wa.connManager.Add(conn)

	// Arm the turn the way a forwarded user message does; then stay silent.
	wa.turns.startTurn(conv)

	select {
	case ev := <-conn.EventChan:
		if ev.Event != EventError {
			t.Fatalf("first broadcast event = %q, want %q", ev.Event, EventError)
		}
	case <-time.After(time.Second):
		t.Fatal("idle watchdog did not finalize the turn")
	}

	got, err := st.Get(ctx, conv)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.AssistantStreaming {
		t.Fatal("conversation still derives assistant_streaming after idle timeout")
	}
	if wa.turns.isStreaming(conv) {
		t.Fatal("turn tracker still reports the turn in flight after idle timeout")
	}
}
