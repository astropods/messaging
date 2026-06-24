package web

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/astropods/messaging/internal/store/sqlite"
)

// Chat-page contract handlers. These serve the platform chat UI (via the
// astro-server /chat/* in-transit proxy) from the sidecar-local SQLite store on
// a shared persistent volume. The JSON shapes match what astro-client expects so
// astro-server can forward responses verbatim. The sidecar has no Langfuse
// access; durability comes from the persistent volume, not trace restore.

const (
	chatDefaultConversationLimit = 100
	chatMaxConversationLimit     = 1000
	chatTitleMaxRunes            = 80
)

type chatMessageResponse struct {
	ID      string `json:"id"`
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatConversationSummary struct {
	ConversationID     string    `json:"conversation_id"`
	Title              string    `json:"title"`
	UpdatedAt          time.Time `json:"updated_at"`
	AssistantStreaming bool      `json:"assistant_streaming,omitempty"`
}

type listChatConversationsResponse struct {
	Conversations []chatConversationSummary `json:"conversations"`
}

type getChatConversationResponse struct {
	ConversationID     string                `json:"conversation_id"`
	Title              string                `json:"title"`
	UpdatedAt          time.Time             `json:"updated_at"`
	Messages           []chatMessageResponse `json:"messages"`
	AssistantStreaming bool                  `json:"assistant_streaming"`
	HasMore            bool                  `json:"has_more,omitempty"`
	OldestSeq          int                   `json:"oldest_seq,omitempty"`
}

type upsertChatConversationInput struct {
	Title string `json:"title"`
}

// HandleListChatConversations handles GET /api/chat/conversations.
func (h *Handlers) HandleListChatConversations(w http.ResponseWriter, r *http.Request) {
	session := h.authenticate(w, r)
	if session == nil {
		return
	}
	if h.chatStore == nil {
		writeJSON(w, http.StatusOK, listChatConversationsResponse{Conversations: []chatConversationSummary{}})
		return
	}

	convs, err := h.chatStore.ListByUser(session.UserID)
	if err != nil {
		slog.Error("[Web] chat list conversations failed", "err", err)
		http.Error(w, "failed to list conversations", http.StatusInternalServerError)
		return
	}

	out := make([]chatConversationSummary, 0, len(convs))
	for _, c := range convs {
		out = append(out, chatConversationSummary{
			ConversationID:     c.ConversationID,
			Title:              c.Title,
			UpdatedAt:          c.UpdatedAt,
			AssistantStreaming: c.AssistantStreaming,
		})
	}
	writeJSON(w, http.StatusOK, listChatConversationsResponse{Conversations: out})
}

// HandleGetChatConversation handles GET /api/chat/conversations/{id}.
func (h *Handlers) HandleGetChatConversation(w http.ResponseWriter, r *http.Request) {
	session := h.authenticate(w, r)
	if session == nil {
		return
	}
	conversationID := r.PathValue("id")
	if conversationID == "" {
		http.Error(w, "Missing conversation ID", http.StatusBadRequest)
		return
	}
	if h.chatStore == nil {
		http.Error(w, "conversation not found", http.StatusNotFound)
		return
	}

	limit, beforeSeq, ok := parseChatPage(r)
	if !ok {
		http.Error(w, "invalid pagination", http.StatusBadRequest)
		return
	}

	conv, err := h.chatStore.Get(conversationID)
	if err != nil {
		slog.Error("[Web] chat get conversation failed", "err", err)
		http.Error(w, "failed to load conversation", http.StatusInternalServerError)
		return
	}
	if conv == nil || conv.UserID != session.UserID {
		http.Error(w, "conversation not found", http.StatusNotFound)
		return
	}

	all, err := h.chatStore.ListMessages(conversationID)
	if err != nil {
		slog.Error("[Web] chat list messages failed", "err", err)
		http.Error(w, "failed to load conversation", http.StatusInternalServerError)
		return
	}

	assistantStreaming := len(all) > 0 && all[len(all)-1].Role == "user"
	messages, hasMore, oldestSeq := paginateChatMessages(all, limit, beforeSeq)

	writeJSON(w, http.StatusOK, getChatConversationResponse{
		ConversationID:     conversationID,
		Title:              conv.Title,
		UpdatedAt:          conv.UpdatedAt,
		Messages:           messages,
		AssistantStreaming: assistantStreaming,
		HasMore:            hasMore,
		OldestSeq:          oldestSeq,
	})
}

// HandleUpsertChatConversation handles PUT /api/chat/conversations/{id}: create
// the row, rename it (non-empty title), or bump recency (empty title = touch).
// The title is durable because it lives on the shared persistent volume.
func (h *Handlers) HandleUpsertChatConversation(w http.ResponseWriter, r *http.Request) {
	session := h.authenticate(w, r)
	if session == nil {
		return
	}
	conversationID := r.PathValue("id")
	if conversationID == "" {
		http.Error(w, "Missing conversation ID", http.StatusBadRequest)
		return
	}
	if h.chatStore == nil {
		writeJSON(w, http.StatusOK, map[string]string{"conversation_id": conversationID})
		return
	}

	var input upsertChatConversationInput
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&input)
	}
	title := strings.TrimSpace(input.Title)
	if utf8.RuneCountInString(title) > 200 {
		http.Error(w, "title too long", http.StatusBadRequest)
		return
	}

	if err := h.chatStore.Upsert(conversationID, session.UserID, title); err != nil {
		slog.Error("[Web] chat upsert conversation failed", "err", err)
		http.Error(w, "failed to save conversation", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"conversation_id": conversationID, "title": title})
}

// HandleDeleteChatConversation handles DELETE /api/chat/conversations/{id}.
//
// Delete is a soft delete: it hides the conversation from the owning user's list.
// Because the store lives on a durable persistent volume, the soft delete sticks
// across pod reschedules (no resurrection).
//
// It intentionally does NOT touch Langfuse. The agent-run traces in Langfuse are
// telemetry that powers cost/usage and observability analytics, which a user must
// not be able to wipe by deleting a chat thread. (The sidecar has no Langfuse
// access in any case.) True right-to-erasure of that telemetry, if needed, is a
// separate policy-gated concern handled outside the chat store.
func (h *Handlers) HandleDeleteChatConversation(w http.ResponseWriter, r *http.Request) {
	session := h.authenticate(w, r)
	if session == nil {
		return
	}
	conversationID := r.PathValue("id")
	if conversationID == "" {
		http.Error(w, "Missing conversation ID", http.StatusBadRequest)
		return
	}
	if h.chatStore == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	deleted, err := h.chatStore.SoftDelete(conversationID, session.UserID)
	if err != nil {
		slog.Error("[Web] chat delete conversation failed", "conversation", conversationID, "err", err)
		http.Error(w, "failed to delete conversation", http.StatusInternalServerError)
		return
	}
	if !deleted {
		http.Error(w, "conversation not found", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func parseChatPage(r *http.Request) (limit, beforeSeq int, ok bool) {
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 1 || v > chatMaxConversationLimit {
			return 0, 0, false
		}
		limit = v
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("before_seq")); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 1 {
			return 0, 0, false
		}
		beforeSeq = v
	}
	return limit, beforeSeq, true
}

// paginateChatMessages returns a page of the ordered thread plus pagination
// markers. Sequence numbers are contiguous from 1, so positional slicing aligns
// with seq. Mirrors the previous astro-server pagination contract.
func paginateChatMessages(all []sqlite.Message, limit, beforeSeq int) (messages []chatMessageResponse, hasMore bool, oldestSeq int) {
	if limit == 0 {
		limit = chatDefaultConversationLimit
	}
	n := len(all)
	if n == 0 {
		return nil, false, 0
	}

	var window []sqlite.Message
	switch {
	case beforeSeq > 0:
		end := beforeSeq - 1
		if end < 1 {
			return nil, false, 0
		}
		start := end - limit + 1
		if start < 1 {
			start = 1
		}
		window = all[start-1 : end]
		hasMore = start > 1
		oldestSeq = start
	case n <= limit:
		window = all
		hasMore = false
		oldestSeq = 1
	default:
		start := n - limit
		window = all[start:]
		hasMore = true
		oldestSeq = start + 1
	}

	messages = make([]chatMessageResponse, 0, len(window))
	for _, m := range window {
		messages = append(messages, chatMessageResponse{ID: m.ID, Role: m.Role, Content: m.Content})
	}
	if len(window) > 0 {
		oldestSeq = window[0].Seq
	}
	return messages, hasMore, oldestSeq
}

func truncateRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max])
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Error("[Web] chat encode response failed", "err", err)
	}
}
