package web

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/astropods/messaging/internal/langfuse"
	"github.com/astropods/messaging/internal/store/sqlite"
)

// Chat-page contract handlers. These serve the platform chat UI (via the
// astro-server /chat/* in-transit proxy) from the sidecar-local SQLite store,
// with Langfuse used to rebuild history when the local store is empty (e.g.
// after a pod reschedule). The JSON shapes match what astro-client expects so
// astro-server can forward responses verbatim.

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
	if len(convs) == 0 {
		h.restoreConversationsFromLangfuse(r.Context(), session.UserID)
		convs, err = h.chatStore.ListByUser(session.UserID)
		if err != nil {
			slog.Error("[Web] chat list conversations failed after restore", "err", err)
			http.Error(w, "failed to list conversations", http.StatusInternalServerError)
			return
		}
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
	// Attempt restore when the conversation is unknown locally (post-reschedule).
	if conv == nil {
		h.restoreThreadFromLangfuse(r.Context(), session.UserID, conversationID, "")
		conv, err = h.chatStore.Get(conversationID)
		if err != nil {
			http.Error(w, "failed to load conversation", http.StatusInternalServerError)
			return
		}
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
	if len(all) == 0 {
		h.restoreThreadFromLangfuse(r.Context(), session.UserID, conversationID, conv.Title)
		all, err = h.chatStore.ListMessages(conversationID)
		if err != nil {
			http.Error(w, "failed to load conversation", http.StatusInternalServerError)
			return
		}
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

	// Persist the title to Langfuse so it survives a pod reschedule (which wipes
	// the local store). Best-effort and off the request path — a failure only
	// means the title falls back to the first message after a redeploy.
	if title != "" && h.langfuse != nil {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := h.langfuse.UpsertSessionTitle(ctx, session.UserID, conversationID, title); err != nil {
				slog.Warn("[Web] chat persist title to langfuse failed", "conversation", conversationID, "err", err)
			}
		}()
	}

	writeJSON(w, http.StatusOK, map[string]string{"conversation_id": conversationID, "title": title})
}

// HandleDeleteChatConversation handles DELETE /api/chat/conversations/{id}.
//
// Delete is a soft delete: it hides the conversation from the owning user's list
// but does NOT erase any Langfuse data.
//
// CONUNDRUM (unresolved — do not "fix" by erasing Langfuse without a decision):
// the Langfuse traces behind a conversation are also the agent-run telemetry that
// powers cost/usage and observability analytics. A user must not be able to wipe
// that telemetry just by deleting a chat thread, so delete deliberately leaves
// Langfuse untouched. The cost of that choice is that a soft delete only hides
// locally — after a pod reschedule wipes the sidecar's SQLite, the conversation
// is rebuilt from Langfuse and reappears.
//
// Candidate resolutions to evaluate later:
//   - Write a "deleted" tombstone to the conversation's Langfuse metadata trace
//     and have restore filter it out (durable hide, analytics preserved).
//   - Separate the user-facing chat record from raw analytics traces so chat
//     content can be erased independently of telemetry.
//   - Real right-to-erasure gated by policy/role, distinct from a casual delete.
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

// restoreThreadFromLangfuse rebuilds one conversation's messages from Langfuse
// when the local store has none. Best-effort: failures leave the thread empty.
func (h *Handlers) restoreThreadFromLangfuse(ctx context.Context, userID, conversationID, title string) {
	if h.langfuse == nil || h.chatStore == nil {
		return
	}
	msgs, persistedTitle, err := h.langfuse.GetSessionMessages(ctx, userID, conversationID)
	if err != nil {
		slog.Warn("[Web] chat langfuse restore (thread) failed", "conversation", conversationID, "err", err)
		return
	}
	if len(msgs) == 0 {
		return
	}
	restored := make([]sqlite.RestoredMessage, 0, len(msgs))
	for _, m := range msgs {
		restored = append(restored, sqlite.RestoredMessage{Role: m.Role, Content: m.Content})
	}
	// Prefer the persisted (user-set) title, then any caller-provided title,
	// then the first user message.
	if persistedTitle != "" {
		title = persistedTitle
	}
	if title == "" {
		title = firstUserTitle(msgs)
	}
	if err := h.chatStore.BackfillMessages(conversationID, userID, title, restored); err != nil {
		slog.Warn("[Web] chat langfuse restore (thread) backfill failed", "conversation", conversationID, "err", err)
	}
}

// restoreConversationsFromLangfuse rebuilds the user's conversation list from
// Langfuse when the local store is empty. Best-effort.
func (h *Handlers) restoreConversationsFromLangfuse(ctx context.Context, userID string) {
	if h.langfuse == nil || h.chatStore == nil {
		return
	}
	sessions, err := h.langfuse.ListUserSessions(ctx, userID)
	if err != nil {
		slog.Warn("[Web] chat langfuse restore (list) failed", "err", err)
		return
	}
	if len(sessions) == 0 {
		return
	}
	convs := make([]sqlite.RestoredConversation, 0, len(sessions))
	for _, s := range sessions {
		convs = append(convs, sqlite.RestoredConversation{
			ConversationID: s.ConversationID,
			Title:          s.Title,
			UpdatedAt:      s.UpdatedAt,
		})
	}
	if err := h.chatStore.BackfillConversations(userID, convs); err != nil {
		slog.Warn("[Web] chat langfuse restore (list) backfill failed", "err", err)
	}
}

func firstUserTitle(msgs []langfuse.ChatMessage) string {
	for _, m := range msgs {
		if m.Role == "user" && strings.TrimSpace(m.Content) != "" {
			return truncateRunes(m.Content, chatTitleMaxRunes)
		}
	}
	return ""
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
