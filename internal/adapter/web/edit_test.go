package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/astropods/messaging/internal/adapter"
	"github.com/astropods/messaging/internal/store"
	"github.com/astropods/messaging/internal/store/sqlite"
	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
)

func editRequest(user, conversationID, content, editOf string) *http.Request {
	body := `{"content":` + strconvQuote(content) + `,"edit_of":` + strconvQuote(editOf) + `}`
	req := httptest.NewRequest(http.MethodPost, "/api/conversations/"+conversationID+"/messages", strings.NewReader(body))
	req.Header.Set("X-User-ID", user)
	req.SetPathValue("id", conversationID)
	return req
}

func editableChat(t *testing.T) (*Handlers, *sqlite.Store, *[]*pb.Message) {
	t.Helper()
	h, st := newChatTitleHandlers(t)
	h.agentConfigStore = historyAgent()
	var forwarded []*pb.Message
	h.SetMessageHandler(func(_ context.Context, msg *pb.Message) error {
		forwarded = append(forwarded, msg)
		return nil
	})
	for _, q := range []string{"q1", "q2"} {
		w := httptest.NewRecorder()
		h.HandleSendMessage(w, sendMessageRequest("user-1", "conv-1", q))
		if w.Code != http.StatusOK {
			t.Fatalf("send %q: %d %s", q, w.Code, w.Body.String())
		}
		if _, err := st.FinishAssistantReply(t.Context(), "conv-1", "a"+q[1:], ""); err != nil {
			t.Fatalf("reply: %v", err)
		}
	}
	return h, st, &forwarded
}

func historyAgent() *store.AgentConfigStore {
	cs := store.NewAgentConfigStore()
	cs.Set(&pb.AgentConfig{SupportsHistory: true})
	return cs
}

func activeBranch(t *testing.T, st *sqlite.Store) []sqlite.Message {
	t.Helper()
	msgs, hasMore, _, _, err := st.PageMessages(t.Context(), "conv-1", 100, 0)
	if err != nil || hasMore {
		t.Fatalf("page: hasMore=%v err=%v, want the whole branch on one page", hasMore, err)
	}
	return msgs
}

func TestASendWithEditOfForwardsTheEarlierTurnsAsHistory(t *testing.T) {
	h, st, forwarded := editableChat(t)
	q2 := activeBranch(t, st)[2]

	w := httptest.NewRecorder()
	h.HandleSendMessage(w, editRequest("user-1", "conv-1", "q2 edited", q2.ID))

	if w.Code != http.StatusOK {
		t.Fatalf("edit: want 200, got %d %s", w.Code, w.Body.String())
	}
	for i, msg := range (*forwarded)[:2] {
		if msg.History != nil {
			t.Errorf("ordinary send %d carried history %v", i, msg.History)
		}
	}
	edit := (*forwarded)[2]
	if edit.GetContent() != "q2 edited" || edit.History == nil {
		t.Fatalf("forwarded edit = %q history=%v, want the edited text with history", edit.GetContent(), edit.History)
	}
	var got []string
	for _, m := range edit.History.GetMessages() {
		got = append(got, m.GetRole()+":"+m.GetContent())
	}
	if want := []string{"user:q1", "assistant:a1"}; !slices.Equal(got, want) {
		t.Errorf("history = %v, want %v", got, want)
	}

	msgs := activeBranch(t, st)
	if len(msgs) != 3 || msgs[2].Content != "q2 edited" || len(msgs[2].Branches) != 2 {
		t.Errorf("active branch after edit = %+v, want q1, a1 and the edit with two branches", msgs)
	}
}

func TestASendWithAnInvalidEditOfIsRejectedBeforeTheAgent(t *testing.T) {
	h, st, forwarded := editableChat(t)
	assistant := activeBranch(t, st)[1]

	for _, editOf := range []string{assistant.ID, "missing"} {
		w := httptest.NewRecorder()
		h.HandleSendMessage(w, editRequest("user-1", "conv-1", "x", editOf))
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "invalid_edit") {
			t.Errorf("edit_of %q: got %d %s, want 400 invalid_edit", editOf, w.Code, w.Body.String())
		}
	}
	if len(*forwarded) != 2 {
		t.Errorf("a rejected edit reached the agent: %d forwards, want 2", len(*forwarded))
	}
}

func TestASendWithEditOfNeedsTheChatStore(t *testing.T) {
	h := NewHandlers(NewConnectionManager(30*time.Second), NewHeaderSessionManager("X-User-ID", "", ""), nil, nil)
	h.SetMessageHandler(func(context.Context, *pb.Message) error {
		t.Fatal("an edit without a chat store must not reach the agent")
		return nil
	})

	w := httptest.NewRecorder()
	h.HandleSendMessage(w, editRequest("user-1", "conv-1", "x", "m1"))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d %s, want 400", w.Code, w.Body.String())
	}
}

func switchBranchRequest(user, conversationID, messageID string) *http.Request {
	req := httptest.NewRequest(http.MethodPut, "/api/chat/conversations/"+conversationID+"/branch",
		strings.NewReader(`{"message_id":`+strconvQuote(messageID)+`}`))
	req.Header.Set("X-User-ID", user)
	return req
}

func TestHandleSwitchChatBranchReturnsTheChosenBranch(t *testing.T) {
	h, st, _ := editableChat(t)
	q2 := activeBranch(t, st)[2]
	w := httptest.NewRecorder()
	h.HandleSendMessage(w, editRequest("user-1", "conv-1", "q2 edited", q2.ID))
	if w.Code != http.StatusOK {
		t.Fatalf("edit: %d %s", w.Code, w.Body.String())
	}

	// Through the mux, so the route registration is covered too.
	w = httptest.NewRecorder()
	(&WebAdapter{handlers: h}).routes().ServeHTTP(w, switchBranchRequest("user-1", "conv-1", q2.ID))

	if w.Code != http.StatusOK {
		t.Fatalf("switch: want 200, got %d %s", w.Code, w.Body.String())
	}
	var resp getChatConversationResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var got []string
	for _, m := range resp.Messages {
		got = append(got, m.Content)
	}
	if want := []string{"q1", "a1", "q2", "a2"}; !slices.Equal(got, want) {
		t.Fatalf("thread after switch = %v, want the original branch %v", got, want)
	}
	if len(resp.Messages[2].Branches) != 2 || resp.Messages[2].Branches[0] != q2.ID {
		t.Errorf("branches = %v, want the original first of two", resp.Messages[2].Branches)
	}
}

func TestHandleSwitchChatBranchNotFound(t *testing.T) {
	h, st, _ := editableChat(t)
	q1 := activeBranch(t, st)[0]

	for name, req := range map[string]*http.Request{
		"unknown message":      switchBranchRequest("user-1", "conv-1", "missing"),
		"foreign conversation": switchBranchRequest("user-2", "conv-1", q1.ID),
	} {
		w := httptest.NewRecorder()
		(&WebAdapter{handlers: h}).routes().ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s: got %d %s, want 404", name, w.Code, w.Body.String())
		}
	}
}

func TestHandleSwitchChatBranchRejectsWhileATurnIsInFlight(t *testing.T) {
	h, st, _ := editableChat(t)
	h.turns = newTurnTracker()
	h.turns.startTurn("conv-1")
	q1 := activeBranch(t, st)[0]

	w := httptest.NewRecorder()
	(&WebAdapter{handlers: h}).routes().ServeHTTP(w, switchBranchRequest("user-1", "conv-1", q1.ID))

	if w.Code != http.StatusConflict {
		t.Fatalf("got %d %s, want 409 turn_in_progress", w.Code, w.Body.String())
	}
}

func TestHandleGetChatConversationListsBranches(t *testing.T) {
	h, st, _ := editableChat(t)
	q2 := activeBranch(t, st)[2]
	w := httptest.NewRecorder()
	h.HandleSendMessage(w, editRequest("user-1", "conv-1", "q2 edited", q2.ID))

	req := httptest.NewRequest(http.MethodGet, "/api/chat/conversations/conv-1", nil)
	req.Header.Set("X-User-ID", "user-1")
	w = httptest.NewRecorder()
	(&WebAdapter{handlers: h}).routes().ServeHTTP(w, req)

	var raw struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := raw.Messages[0]["branches"]; ok {
		t.Error("a message without alternatives must omit branches")
	}
	if b, ok := raw.Messages[2]["branches"].([]any); !ok || len(b) != 2 {
		t.Errorf("edited message branches = %v, want two ids", raw.Messages[2]["branches"])
	}
}

func TestHandleAgentConfigReportsEditCapability(t *testing.T) {
	editCap := func(chatStore, declared bool) bool {
		cs := store.NewAgentConfigStore()
		cs.Set(&pb.AgentConfig{SystemPrompt: "sp", SupportsHistory: declared})
		h := NewHandlers(NewConnectionManager(time.Second), &NoopSessionManager{}, nil, cs)
		if chatStore {
			_, st := newChatTitleHandlers(t)
			h.chatStore = st
		}
		w := httptest.NewRecorder()
		h.HandleAgentConfig(w, httptest.NewRequest(http.MethodGet, "/api/agent/config", nil))
		var resp struct {
			Capabilities struct {
				Edit bool `json:"edit"`
			} `json:"capabilities"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode agent/config: %v", err)
		}
		return resp.Capabilities.Edit
	}

	for _, tc := range []struct{ chatStore, declared, want bool }{
		{true, true, true},
		{true, false, false},
		{false, true, false},
		{false, false, false},
	} {
		if got := editCap(tc.chatStore, tc.declared); got != tc.want {
			t.Errorf("edit(chatStore=%v, declared=%v) = %v, want %v", tc.chatStore, tc.declared, got, tc.want)
		}
	}
}

func TestThreadStoreKeepsAReplyRoutedDuringASynchronousForward(t *testing.T) {
	h, st := newChatTitleHandlers(t)
	ts := store.NewThreadHistoryStore(10, 50, time.Hour)
	h.threadStore = ts
	a := &WebAdapter{connManager: NewConnectionManager(30 * time.Second), chatStore: st, threadStore: ts}

	// A failed forward leaves the agent head behind, so the next send carries history.
	h.SetMessageHandler(func(context.Context, *pb.Message) error { return adapter.ErrNoAgentStream })
	w := httptest.NewRecorder()
	h.HandleSendMessage(w, sendMessageRequest("user-1", "conv-1", "q1"))
	if w.Code != http.StatusFailedDependency {
		t.Fatalf("q1: want 424, got %d", w.Code)
	}

	h.SetMessageHandler(func(ctx context.Context, msg *pb.Message) error {
		if msg.History == nil {
			t.Fatal("precondition: q2 must carry history")
		}
		return a.HandleAgentResponse(ctx, &pb.AgentResponse{
			ConversationId: "conv-1",
			ResponseId:     "r2",
			Payload:        &pb.AgentResponse_Content{Content: &pb.ContentChunk{Type: pb.ContentChunk_END, Content: "a2"}},
		})
	})
	w = httptest.NewRecorder()
	h.HandleSendMessage(w, sendMessageRequest("user-1", "conv-1", "q2"))
	if w.Code != http.StatusOK {
		t.Fatalf("q2: want 200, got %d %s", w.Code, w.Body.String())
	}

	var got []string
	for _, m := range ts.GetHistory("conv-1", 50, false).GetMessages() {
		got = append(got, m.GetContent())
	}
	if !slices.Contains(got, "a2") {
		t.Errorf("thread store = %v, want it to keep the agent's reply a2", got)
	}
}

func anchoredRequest(user, conversationID, content, parentID string) *http.Request {
	body := `{"content":` + strconvQuote(content) + `,"parent_id":` + strconvQuote(parentID) + `}`
	req := httptest.NewRequest(http.MethodPost, "/api/conversations/"+conversationID+"/messages", strings.NewReader(body))
	req.Header.Set("X-User-ID", user)
	req.SetPathValue("id", conversationID)
	return req
}

func TestASendAnchoredOnAStaleBranchIsRefusedWith409(t *testing.T) {
	h, st, forwarded := editableChat(t)
	branch := activeBranch(t, st)
	staleHead := branch[3].ID
	w := httptest.NewRecorder()
	h.HandleSendMessage(w, editRequest("user-1", "conv-1", "q2 edited", branch[2].ID))
	if w.Code != http.StatusOK {
		t.Fatalf("edit: %d %s", w.Code, w.Body.String())
	}
	if _, err := st.FinishAssistantReply(t.Context(), "conv-1", "a2 edited", ""); err != nil {
		t.Fatalf("reply: %v", err)
	}
	sent := len(*forwarded)

	w = httptest.NewRecorder()
	h.HandleSendMessage(w, anchoredRequest("user-1", "conv-1", "q3", staleHead))

	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "stale_branch") {
		t.Fatalf("got %d %s, want 409 stale_branch for a send anchored on the old branch", w.Code, w.Body.String())
	}
	if len(*forwarded) != sent {
		t.Error("a stale send reached the agent")
	}

	w = httptest.NewRecorder()
	h.HandleSendMessage(w, anchoredRequest("user-1", "conv-1", "q3", activeBranch(t, st)[3].ID))
	if w.Code != http.StatusOK {
		t.Errorf("a send anchored on the active head: got %d %s, want 200", w.Code, w.Body.String())
	}
}

func TestEditsAndBranchSwitchesNeedAnAgentThatSupportsHistory(t *testing.T) {
	h, st, forwarded := editableChat(t)
	h.agentConfigStore = store.NewAgentConfigStore()
	h.agentConfigStore.Set(&pb.AgentConfig{})
	q2 := activeBranch(t, st)[2]

	w := httptest.NewRecorder()
	h.HandleSendMessage(w, editRequest("user-1", "conv-1", "q2 edited", q2.ID))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "edit_unsupported") {
		t.Errorf("edit: got %d %s, want 400 edit_unsupported", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	(&WebAdapter{handlers: h}).routes().ServeHTTP(w, switchBranchRequest("user-1", "conv-1", q2.ID))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "edit_unsupported") {
		t.Errorf("switch: got %d %s, want 400 edit_unsupported", w.Code, w.Body.String())
	}
	if len(*forwarded) != 2 {
		t.Errorf("a refused edit reached the agent: %d forwards, want 2", len(*forwarded))
	}
}

func TestASendOrSwitchIsRefusedWhileAnotherHoldsTheConversation(t *testing.T) {
	h, st, _ := editableChat(t)
	h.turns = newTurnTracker()
	q1 := activeBranch(t, st)[0]
	if !h.turns.claim("conv-1") {
		t.Fatal("precondition: claim the conversation as an in-progress send would")
	}

	w := httptest.NewRecorder()
	h.HandleSendMessage(w, sendMessageRequest("user-1", "conv-1", "q3"))
	if w.Code != http.StatusConflict {
		t.Errorf("send: got %d, want 409 while another send holds the conversation", w.Code)
	}
	w = httptest.NewRecorder()
	(&WebAdapter{handlers: h}).routes().ServeHTTP(w, switchBranchRequest("user-1", "conv-1", q1.ID))
	if w.Code != http.StatusConflict {
		t.Errorf("switch: got %d, want 409 while a send holds the conversation", w.Code)
	}

	h.turns.release("conv-1")
	w = httptest.NewRecorder()
	(&WebAdapter{handlers: h}).routes().ServeHTTP(w, switchBranchRequest("user-1", "conv-1", q1.ID))
	if w.Code != http.StatusOK {
		t.Errorf("switch after release: got %d %s, want 200", w.Code, w.Body.String())
	}
}

func TestOnlyTheENDChunkRecordsTheAgentsReply(t *testing.T) {
	h, st := newChatTitleHandlers(t)
	a := &WebAdapter{connManager: NewConnectionManager(30 * time.Second), chatStore: st}
	var forwarded []*pb.Message
	h.SetMessageHandler(func(_ context.Context, msg *pb.Message) error {
		forwarded = append(forwarded, msg)
		return nil
	})
	reply := func(kind pb.ContentChunk_ChunkType, text string) {
		if err := a.HandleAgentResponse(t.Context(), &pb.AgentResponse{
			ConversationId: "conv-1",
			Payload:        &pb.AgentResponse_Content{Content: &pb.ContentChunk{Type: kind, Content: text}},
		}); err != nil {
			t.Fatalf("agent response: %v", err)
		}
	}
	send := func(content string) {
		w := httptest.NewRecorder()
		h.HandleSendMessage(w, sendMessageRequest("user-1", "conv-1", content))
		if w.Code != http.StatusOK {
			t.Fatalf("send %q: %d %s", content, w.Code, w.Body.String())
		}
	}

	send("q1")
	reply(pb.ContentChunk_END, "a1")
	send("q2")
	if forwarded[1].History != nil {
		t.Errorf("q2 follows a completed reply but carried history %v", forwarded[1].History)
	}
	reply(pb.ContentChunk_DELTA, "partial")
	send("q3")
	if forwarded[2].History == nil {
		t.Error("q3 follows a reply that never reached END, so it must carry history")
	}
}
