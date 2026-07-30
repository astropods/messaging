package web

// Locks in the scoped chat title-rename endpoint (PUT
// /api/chat/conversations/{id}/title). It is a scoped PUT on the /title
// sub-resource (idempotent, rename-only), replacing an earlier overloaded PUT on
// the whole conversation — these tests pin that it now ONLY renames an existing,
// caller-owned conversation and cannot be used to create, touch, or reach another
// user's thread.
//
//	go test ./internal/adapter/web -run TestHandleSetChatConversationTitle -v

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/astropods/messaging/internal/adapter"
	"github.com/astropods/messaging/internal/store/sqlite"
	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
)

// newChatTitleHandlers builds Handlers wired to a fresh temp SQLite chat store
// and a header-based session manager (X-User-ID selects the caller; absent ->
// unauthenticated).
func newChatTitleHandlers(t *testing.T) (*Handlers, *sqlite.Store) {
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

func setTitleRequest(user, conversationID, title string) *http.Request {
	req := httptest.NewRequest(http.MethodPut,
		"/api/chat/conversations/"+conversationID+"/title",
		strings.NewReader(`{"title":`+strconvQuote(title)+`}`))
	if user != "" {
		req.Header.Set("X-User-ID", user)
	}
	req.SetPathValue("id", conversationID)
	return req
}

// strconvQuote JSON-quotes a string for the request body.
func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestHandleSetChatConversationTitle_RenamesOwnedConversation(t *testing.T) {
	h, st := newChatTitleHandlers(t)
	if err := st.Upsert(t.Context(), "conv-1", "user-1", "original"); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}

	w := httptest.NewRecorder()
	h.HandleSetChatConversationTitle(w, setTitleRequest("user-1", "conv-1", "Renamed"))

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%q", w.Code, w.Body.String())
	}
	conv, err := st.Get(t.Context(), "conv-1")
	if err != nil || conv == nil {
		t.Fatalf("get conv: conv=%v err=%v", conv, err)
	}
	if conv.Title != "Renamed" {
		t.Errorf("title = %q, want %q", conv.Title, "Renamed")
	}
}

func TestHandleSetChatConversationTitle_NoSession_401(t *testing.T) {
	h, st := newChatTitleHandlers(t)
	if err := st.Upsert(t.Context(), "conv-1", "user-1", "original"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	w := httptest.NewRecorder()
	h.HandleSetChatConversationTitle(w, setTitleRequest("", "conv-1", "Renamed")) // no X-User-ID

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d body=%q", w.Code, w.Body.String())
	}
	// Title must be untouched.
	conv, _ := st.Get(t.Context(), "conv-1")
	if conv == nil || conv.Title != "original" {
		t.Errorf("title changed by unauthenticated request: %+v", conv)
	}
}

func TestHandleSetChatConversationTitle_EmptyTitle_400(t *testing.T) {
	h, st := newChatTitleHandlers(t)
	if err := st.Upsert(t.Context(), "conv-1", "user-1", "original"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	for _, title := range []string{"", "   "} {
		w := httptest.NewRecorder()
		h.HandleSetChatConversationTitle(w, setTitleRequest("user-1", "conv-1", title))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("title=%q: want 400, got %d body=%q", title, w.Code, w.Body.String())
		}
	}
}

func TestHandleSetChatConversationTitle_TooLong_400(t *testing.T) {
	h, st := newChatTitleHandlers(t)
	if err := st.Upsert(t.Context(), "conv-1", "user-1", "original"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	long := strings.Repeat("x", chatTitleMaxRunes+1)
	w := httptest.NewRecorder()
	h.HandleSetChatConversationTitle(w, setTitleRequest("user-1", "conv-1", long))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d body=%q", w.Code, w.Body.String())
	}
}

func TestHandleSetChatConversationTitle_NotOwner_404(t *testing.T) {
	h, st := newChatTitleHandlers(t)
	// Conversation belongs to user-2.
	if err := st.Upsert(t.Context(), "conv-1", "user-2", "original"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	w := httptest.NewRecorder()
	h.HandleSetChatConversationTitle(w, setTitleRequest("user-1", "conv-1", "Hijacked"))

	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d body=%q", w.Code, w.Body.String())
	}
	// user-2's title must be untouched (no cross-user rename or existence leak).
	conv, _ := st.Get(t.Context(), "conv-1")
	if conv == nil || conv.Title != "original" {
		t.Errorf("foreign conversation was modified: %+v", conv)
	}
}

func TestHandleSetChatConversationTitle_Missing_404_DoesNotCreate(t *testing.T) {
	h, st := newChatTitleHandlers(t)

	w := httptest.NewRecorder()
	h.HandleSetChatConversationTitle(w, setTitleRequest("user-1", "conv-missing", "New"))

	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d body=%q", w.Code, w.Body.String())
	}
	// The endpoint must not create a conversation as a side effect.
	conv, _ := st.Get(t.Context(), "conv-missing")
	if conv != nil {
		t.Errorf("title endpoint created a conversation: %+v", conv)
	}
}

func sendMessageRequest(user, conversationID, content string) *http.Request {
	req := httptest.NewRequest(http.MethodPost,
		"/api/conversations/"+conversationID+"/messages",
		strings.NewReader(`{"content":`+strconvQuote(content)+`}`))
	if user != "" {
		req.Header.Set("X-User-ID", user)
	}
	req.SetPathValue("id", conversationID)
	return req
}

// The user turn must be persisted before the message is forwarded to the agent:
// HandleAgentResponse runs on another goroutine, so a fast agent that reaches
// END before the user write lands would otherwise invert turn order or — since
// UpsertAssistantProgress no-ops on a not-yet-created conversation — drop the
// assistant reply. The message handler here inspects the store at forward time.
func TestHandleSendMessagePersistsUserBeforeForwarding(t *testing.T) {
	h, st := newChatTitleHandlers(t)

	var userRowVisibleAtForward bool
	h.SetMessageHandler(func(ctx context.Context, msg *pb.Message) error {
		msgs, err := st.ListMessages(ctx, msg.ConversationId)
		if err != nil {
			return err
		}
		for _, m := range msgs {
			if m.Role == "user" {
				userRowVisibleAtForward = true
			}
		}
		return nil
	})

	w := httptest.NewRecorder()
	h.HandleSendMessage(w, sendMessageRequest("user-1", "conv-1", "hello world"))

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%q", w.Code, w.Body.String())
	}
	if !userRowVisibleAtForward {
		t.Fatal("user message must be persisted before forwarding to the agent")
	}
	// First send also creates and titles the conversation.
	conv, _ := st.Get(t.Context(), "conv-1")
	if conv == nil || conv.Title != "hello world" {
		t.Fatalf("conversation not created/titled on first send: %+v", conv)
	}
}

// A send to a conversation owned by another user is rejected with 404 — before
// the message reaches the agent — and injects no stored message. Closes the
// cross-user stored-write / injected-turn vector.
func TestHandleSendMessageForeignConversationRejected(t *testing.T) {
	h, st := newChatTitleHandlers(t)
	// conv-1 belongs to user-2.
	if _, err := st.EnsureForSend(t.Context(), "conv-1", "user-2", "owner"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var forwarded bool
	h.SetMessageHandler(func(context.Context, *pb.Message) error {
		forwarded = true
		return nil
	})

	w := httptest.NewRecorder()
	h.HandleSendMessage(w, sendMessageRequest("user-1", "conv-1", "injected"))

	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404 for foreign conversation, got %d body=%q", w.Code, w.Body.String())
	}
	if forwarded {
		t.Fatal("a foreign send must not reach the agent")
	}
	// No message was injected into user-2's thread.
	msgs, _ := st.ListMessages(t.Context(), "conv-1")
	if len(msgs) != 0 {
		t.Fatalf("foreign send injected a stored message: %+v", msgs)
	}
}

// When forwarding to the agent fails after the user turn is persisted, the turn
// must be finalized so the thread doesn't derive assistant_streaming forever
// (latest row would otherwise stay the user's). Mirrors the agent-error path.
func TestHandleSendMessageForwardFailureFinalizes(t *testing.T) {
	h, st := newChatTitleHandlers(t)
	h.SetMessageHandler(func(context.Context, *pb.Message) error {
		return errors.New("agent unreachable")
	})

	w := httptest.NewRecorder()
	h.HandleSendMessage(w, sendMessageRequest("user-1", "conv-1", "hello"))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500 on forward failure, got %d body=%q", w.Code, w.Body.String())
	}
	// The last row must be a terminal assistant row, not the user's — otherwise
	// assistant_streaming derives true forever.
	msgs, err := st.ListMessages(t.Context(), "conv-1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(msgs) != 2 || msgs[len(msgs)-1].Role != "assistant" {
		t.Fatalf("forward failure did not finalize the turn (last row must be assistant): %+v", msgs)
	}
}

// A send that fails only because no agent is connected returns 424 (not 5xx) and
// still finalizes the turn so the thread doesn't hang streaming.
func TestHandleSendMessageNoAgentReturns424(t *testing.T) {
	h, st := newChatTitleHandlers(t)
	h.SetMessageHandler(func(context.Context, *pb.Message) error {
		return adapter.ErrNoAgentStream
	})

	w := httptest.NewRecorder()
	h.HandleSendMessage(w, sendMessageRequest("user-1", "conv-1", "hello"))

	if w.Code != http.StatusFailedDependency {
		t.Fatalf("want 424 when no agent is connected, got %d body=%q", w.Code, w.Body.String())
	}
	msgs, err := st.ListMessages(t.Context(), "conv-1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(msgs) != 2 || msgs[len(msgs)-1].Role != "assistant" {
		t.Fatalf("no-agent send did not finalize the turn (last row must be assistant): %+v", msgs)
	}
}

// Hitting the per-conversation message cap is a terminal, user-actionable state,
// not a transient failure: the send returns 409 (not 503) and does not forward.
func TestHandleSendMessageAtCapReturns409(t *testing.T) {
	h, st := newChatTitleHandlers(t)
	if _, err := st.EnsureForSend(t.Context(), "conv-1", "user-1", "t"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Fill to the reserved cap (user turns hold back the final slot for a reply);
	// the next user send then has no room and must 409.
	for i := 0; i < sqlite.MaxMessagesPerConversation-1; i++ {
		if _, err := st.AppendMessage(t.Context(), "conv-1", "user-1", "user", "m", ""); err != nil {
			t.Fatalf("seed fill %d: %v", i, err)
		}
	}
	var forwarded bool
	h.SetMessageHandler(func(context.Context, *pb.Message) error {
		forwarded = true
		return nil
	})

	w := httptest.NewRecorder()
	h.HandleSendMessage(w, sendMessageRequest("user-1", "conv-1", "over the cap"))

	if w.Code != http.StatusConflict {
		t.Fatalf("want 409 at message cap, got %d body=%q", w.Code, w.Body.String())
	}
	// The client keys the "message limit" toast on this machine-readable code,
	// not the bare 409 — lock in the contract.
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("cap response is not JSON: %v (body=%q)", err, w.Body.String())
	}
	if body.Error != "message_limit_reached" {
		t.Fatalf("cap error code = %q, want message_limit_reached", body.Error)
	}
	if forwarded {
		t.Fatal("must not forward to the agent once the conversation is at its cap")
	}
}

func cancelRequest(user, conversationID string) *http.Request {
	req := httptest.NewRequest(http.MethodPost,
		"/api/conversations/"+conversationID+"/cancel", nil)
	if user != "" {
		req.Header.Set("X-User-ID", user)
	}
	req.SetPathValue("id", conversationID)
	return req
}

// Cancel enforces conversation ownership like every other write path: a foreign
// cancel is rejected with 404 and has NO side effects — it must not stop the
// victim's in-flight turn or forward a STOP to their agent.
func TestHandleCancelForeignConversationRejected(t *testing.T) {
	h, st := newChatTitleHandlers(t)
	h.turns = newTurnTracker()
	// conv-1 belongs to user-2, with an in-flight turn streaming.
	if _, err := st.EnsureForSend(t.Context(), "conv-1", "user-2", "owner"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	h.turns.record("conv-1", &pb.ContentChunk{Type: pb.ContentChunk_START, Content: "hi"})

	var stopSignaled bool
	h.SetFeedbackHandler(func(context.Context, *pb.PlatformFeedback) error {
		stopSignaled = true
		return nil
	})

	w := httptest.NewRecorder()
	h.HandleCancel(w, cancelRequest("user-1", "conv-1"))

	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404 for foreign cancel, got %d body=%q", w.Code, w.Body.String())
	}
	if !h.turns.isStreaming("conv-1") {
		t.Fatal("foreign cancel stopped the victim's in-flight turn")
	}
	if stopSignaled {
		t.Fatal("foreign cancel forwarded a STOP to the victim's agent")
	}
}

// The SSE stream is a live read: a foreign conversation is rejected with 404
// before any stream is established, matching the send/cancel/get boundary. Closes
// the cross-user live-read vector.
func TestHandleStreamForeignConversationRejected(t *testing.T) {
	h, st := newChatTitleHandlers(t)
	// conv-1 belongs to user-2.
	if _, err := st.EnsureForSend(t.Context(), "conv-1", "user-2", "owner"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/conversations/conv-1/stream", nil)
	req.Header.Set("X-User-ID", "user-1")
	req.SetPathValue("id", "conv-1")
	w := httptest.NewRecorder()
	h.HandleStream(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404 for foreign stream, got %d body=%q", w.Code, w.Body.String())
	}
}

// A stream for a conversation that does not exist is also 404 (no existence leak).
func TestHandleStreamMissingConversationRejected(t *testing.T) {
	h, _ := newChatTitleHandlers(t)

	req := httptest.NewRequest(http.MethodGet, "/api/conversations/nope/stream", nil)
	req.Header.Set("X-User-ID", "user-1")
	req.SetPathValue("id", "nope")
	w := httptest.NewRecorder()
	h.HandleStream(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404 for missing stream, got %d body=%q", w.Code, w.Body.String())
	}
}

// The owner can cancel: 204, the turn is stopped (gate set), and the partial is
// finalized so the thread no longer derives as streaming.
func TestHandleCancelOwnedConversation(t *testing.T) {
	h, st := newChatTitleHandlers(t)
	h.turns = newTurnTracker()
	if _, err := st.EnsureForSend(t.Context(), "conv-1", "user-1", "owner"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := st.AppendMessage(t.Context(), "conv-1", "user-1", "user", "q", ""); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	h.turns.record("conv-1", &pb.ContentChunk{Type: pb.ContentChunk_START, Content: "partial reply"})

	w := httptest.NewRecorder()
	h.HandleCancel(w, cancelRequest("user-1", "conv-1"))

	if w.Code != http.StatusNoContent {
		t.Fatalf("want 204 for owner cancel, got %d body=%q", w.Code, w.Body.String())
	}
	if h.turns.isStreaming("conv-1") {
		t.Fatal("owner cancel did not stop the turn")
	}
}

// The ownership check is an authorization boundary: a store error must fail
// closed (503) and never forward to the agent with an unverified conversation id.
func TestHandleSendMessageFailsClosedOnStoreError(t *testing.T) {
	h, st := newChatTitleHandlers(t)
	var forwarded bool
	h.SetMessageHandler(func(context.Context, *pb.Message) error {
		forwarded = true
		return nil
	})
	_ = st.Close() // subsequent store calls now error (db closed)

	w := httptest.NewRecorder()
	h.HandleSendMessage(w, sendMessageRequest("user-1", "conv-1", "hi"))

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503 on store error, got %d body=%q", w.Code, w.Body.String())
	}
	if forwarded {
		t.Fatal("must not forward to the agent when the ownership check errored")
	}
}
