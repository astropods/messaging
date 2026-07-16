package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/astropods/messaging/internal/store/sqlite"
)

// These tests cover the "stuck animated avatar" chat report: a turn completes
// and persists on the sidecar, the reply shows in the trace, but the client
// stays in its loading state until a manual refresh.
//
// Root cause: Broadcast (connection.go) fans out only to connections registered
// at emit time and buffers nothing, and HandleStream subscribes a fresh
// connection. So a turn that finishes in the window between the send and the
// EventSource registering broadcasts its finish to nobody, and the late
// subscriber never learns the turn ended (the client half — a turn whose SSE
// never delivers a finish stays in flight until the 3-min watchdog — is covered
// by use-deployment-chat.test.tsx "watchdog reaps a stalled turn").
//
// Fix: on subscribe, HandleStream replays a terminal finish when the store
// shows the turn already ended, so a late subscriber is reconciled immediately.

// streamHandlers builds Handlers in the dev configuration the cancel tests use:
// a valid no-op session and no chat store, so HandleStream runs its subscribe
// path without the ownership gate. With no store, streamTurnTerminal can't know
// the turn ended, so no replay happens — used by the ordering control below.
func streamHandlers() *Handlers {
	cm := NewConnectionManager(30 * time.Second)
	return NewHandlers(cm, &NoopSessionManager{}, nil, nil)
}

// streamHandlersWithStore wires a fresh temp SQLite chat store and a
// header-based session manager (X-User-ID selects the caller), so the terminal-
// state replay path can be exercised against real persisted turn state.
func streamHandlersWithStore(t *testing.T) (*Handlers, *sqlite.Store) {
	t.Helper()
	cm := NewConnectionManager(30 * time.Second)
	sm := NewHeaderSessionManager("X-User-ID", "", "")
	h := NewHandlers(cm, sm, nil, nil)
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "chat.db"))
	if err != nil {
		t.Fatalf("open sqlite store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	h.chatStore = st
	return h, st
}

// waitForConnectionOrDone blocks until HandleStream has registered its SSE
// connection (so a broadcast in onOpen races nothing) or the handler has already
// returned (the terminal-replay path adds, replays a finish, and removes the
// connection synchronously, so it may never be observed as registered).
func waitForConnectionOrDone(t *testing.T, h *Handlers, convID string, done <-chan struct{}) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-done:
			return
		default:
		}
		if h.connManager.GetConnectionCount(convID) > 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("stream connection for %q never registered and handler did not return", convID)
}

// runStream drives HandleStream for a bounded window and returns everything it
// wrote. The handler blocks in its event loop until the request context is
// cancelled (a real client disconnect); onOpen (if set) fires once the
// connection is registered. `user` sets X-User-ID when the handler is store-
// backed (ownership check). The body is read only after the goroutine returns,
// so there is no concurrent access to the recorder.
func runStream(t *testing.T, h *Handlers, convID, user string, onOpen func()) string {
	t.Helper()
	return runStreamResume(t, h, convID, user, "", onOpen)
}

// runStreamResume is runStream with an optional Last-Event-ID (the SSE resume
// cursor an EventSource replays on reconnect); "" means a fresh subscribe.
func runStreamResume(t *testing.T, h *Handlers, convID, user, lastEventID string, onOpen func()) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req := httptest.NewRequest(http.MethodGet, "/api/conversations/"+convID+"/stream", nil)
	req.SetPathValue("id", convID)
	if user != "" {
		req.Header.Set("X-User-ID", user)
	}
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		h.HandleStream(w, req)
		close(done)
	}()

	waitForConnectionOrDone(t, h, convID, done)
	select {
	case <-done:
		// Handler already returned (terminal replay path) — nothing to broadcast.
		return w.Body.String()
	default:
	}
	if onOpen != nil {
		onOpen()
	}
	// Let a post-subscribe broadcast (or the synchronous terminal replay) be
	// pulled from the channel and flushed before we tear the stream down.
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done
	return w.Body.String()
}

// Regression guard for the fix: a client that subscribes after the turn already
// finished (the assistant reply is the latest persisted message) is replayed a
// terminal finish, so it leaves its loading state instead of hanging until the
// watchdog. Before the fix this stream delivered only `connected`.
func TestHandleStream_ReplaysFinishForAlreadyFinishedTurn(t *testing.T) {
	h, st := streamHandlersWithStore(t)
	const (
		convID = "conv-fast-reply"
		user   = "user-1"
	)
	ctx := t.Context()
	if err := st.Upsert(ctx, convID, user, "chat"); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	if _, err := st.AppendMessage(ctx, convID, user, "user", "hi"); err != nil {
		t.Fatalf("seed user message: %v", err)
	}
	// The turn completed and its reply persisted before this client subscribed —
	// the finish broadcast (if any) landed on zero connections and was dropped.
	if _, err := st.AppendMessage(ctx, convID, user, "assistant", "the reply"); err != nil {
		t.Fatalf("seed assistant reply: %v", err)
	}

	body := runStream(t, h, convID, user, nil)

	if !strings.Contains(body, "event: "+EventConnected) {
		t.Fatalf("expected a connected event on subscribe, got:\n%s", body)
	}
	if !strings.Contains(body, "event: "+EventFinish) {
		t.Fatalf("late subscriber to a finished turn must be replayed a finish, got:\n%s", body)
	}
}

// A turn still in flight — latest persisted message is the user's, awaiting the
// first assistant chunk — must NOT be replayed a finish; ending it early would
// drop the real reply. The live broadcast is what finishes such a turn.
func TestHandleStream_NoReplayWhileTurnInFlight(t *testing.T) {
	h, st := streamHandlersWithStore(t)
	const (
		convID = "conv-pending"
		user   = "user-1"
	)
	ctx := t.Context()
	if err := st.Upsert(ctx, convID, user, "chat"); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	if _, err := st.AppendMessage(ctx, convID, user, "user", "hi"); err != nil {
		t.Fatalf("seed user message: %v", err)
	}

	body := runStream(t, h, convID, user, nil)

	if !strings.Contains(body, "event: "+EventConnected) {
		t.Fatalf("expected a connected event on subscribe, got:\n%s", body)
	}
	if strings.Contains(body, "event: "+EventFinish) {
		t.Fatalf("an in-flight turn must not be finished early on subscribe, got:\n%s", body)
	}
}

// Control: the same code delivers the finish when the client subscribes before
// the broadcast. This isolates the original defect to ordering — the client is
// stranded only when the turn finishes inside the subscribe window (no store to
// replay from here), which is why the report was intermittent rather than
// constant.
func TestHandleStream_FinishAfterSubscribeIsDelivered(t *testing.T) {
	h := streamHandlers()
	const convID = "conv-normal-reply"

	body := runStream(t, h, convID, "", func() {
		h.connManager.Broadcast(convID, NewFinishEvent("resp-1"))
	})

	if !strings.Contains(body, "event: "+EventFinish) {
		t.Fatalf("a subscriber connected before the finish must receive it, got:\n%s", body)
	}
}
