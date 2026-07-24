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

	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
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
	ID          string           `json:"id"`
	Role        string           `json:"role"`
	Content     string           `json:"content"`
	Attachments []chatAttachment `json:"attachments,omitempty"`
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
	ConversationID      string                 `json:"conversation_id"`
	Title               string                 `json:"title"`
	UpdatedAt           time.Time              `json:"updated_at"`
	Messages            []chatMessageResponse  `json:"messages"`
	AssistantStreaming  bool                   `json:"assistant_streaming"`
	HasMore             bool                   `json:"has_more,omitempty"`
	OldestSeq           int                    `json:"oldest_seq,omitempty"`
	PendingInteractions []InteractionEventData `json:"pending_interactions,omitempty"`
}

type setChatTitleInput struct {
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

	convs, err := h.chatStore.ListByUser(r.Context(), session.UserID)
	if err != nil {
		slog.Error("[Web] chat list conversations failed", "err", err)
		http.Error(w, "failed to list conversations", http.StatusInternalServerError)
		return
	}

	out := make([]chatConversationSummary, 0, len(convs))
	for _, c := range convs {
		// A turn actively streaming in this sidecar overrides the persisted-state
		// heuristic: the assistant row is written progressively, so the store
		// alone can't tell an in-flight turn from a finished one.
		streaming := c.AssistantStreaming || (h.turns != nil && h.turns.isStreaming(c.ConversationID))
		out = append(out, chatConversationSummary{
			ConversationID:     c.ConversationID,
			Title:              c.Title,
			UpdatedAt:          c.UpdatedAt,
			AssistantStreaming: streaming,
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

	conv, err := h.chatStore.Get(r.Context(), conversationID)
	if err != nil {
		slog.Error("[Web] chat get conversation failed", "err", err)
		http.Error(w, "failed to load conversation", http.StatusInternalServerError)
		return
	}
	if conv == nil || conv.UserID != session.UserID {
		http.Error(w, "conversation not found", http.StatusNotFound)
		return
	}

	if limit == 0 {
		limit = chatDefaultConversationLimit
	}
	window, hasMore, oldestSeq, lastRole, err := h.chatStore.PageMessages(r.Context(), conversationID, limit, beforeSeq)
	if err != nil {
		slog.Error("[Web] chat page messages failed", "err", err)
		http.Error(w, "failed to load conversation", http.StatusInternalServerError)
		return
	}

	// A turn is in flight when the latest persisted message is still the user's
	// (awaiting the first assistant chunk) OR this sidecar is actively streaming
	// the assistant reply. The second clause is required because the assistant
	// row is persisted progressively, so "latest message is the user's" alone no
	// longer distinguishes an in-flight turn from a finished one. lastRole is the
	// newest message's role for the whole thread, independent of this page.
	assistantStreaming := lastRole == "user" ||
		(h.turns != nil && h.turns.isStreaming(conversationID))

	messages := make([]chatMessageResponse, 0, len(window))
	for _, m := range window {
		messages = append(messages, chatMessageResponse{
			ID:          m.ID,
			Role:        m.Role,
			Content:     m.Content,
			Attachments: unmarshalAttachments(m.Attachments),
		})
	}

	writeJSON(w, http.StatusOK, getChatConversationResponse{
		ConversationID:      conversationID,
		Title:               conv.Title,
		UpdatedAt:           conv.UpdatedAt,
		Messages:            messages,
		AssistantStreaming:  assistantStreaming,
		HasMore:             hasMore,
		OldestSeq:           oldestSeq,
		PendingInteractions: h.pendingInteractions(r.Context(), conversationID),
	})
}

// pendingInteractions returns the conversation's still-open blocking interactions
// (the FIFO queue) in the client shape, so a reloaded client re-renders open forms
// and re-enters waiting-for-input. A malformed stored Renderable is skipped rather
// than failing the whole fetch. Empty when no interaction store is wired.
func (h *Handlers) pendingInteractions(ctx context.Context, conversationID string) []InteractionEventData {
	if h.interactions == nil {
		return nil
	}
	pending, err := h.interactions.PendingInteractions(ctx, conversationID)
	if err != nil {
		slog.Error("[Web] chat pending interactions failed", "conversation", conversationID, "err", err)
		return nil
	}
	out := make([]InteractionEventData, 0, len(pending))
	for _, it := range pending {
		data, err := interactionEventData(it.Renderable)
		if err != nil {
			slog.Error("[Web] skipping malformed pending interaction", "conversation", conversationID, "err", err)
			continue
		}
		out = append(out, data)
	}
	return out
}

// HandleSetChatConversationTitle handles PUT /api/chat/conversations/{id}/title.
//
// It renames an existing conversation owned by the caller — and does nothing
// else. It intentionally cannot create a conversation (that happens on create /
// first send) or mutate any other part of the thread, so the title is the only
// user-editable field. A missing/foreign/deleted conversation returns 404.
func (h *Handlers) HandleSetChatConversationTitle(w http.ResponseWriter, r *http.Request) {
	session := h.authenticate(w, r)
	if session == nil {
		return
	}
	conversationID := r.PathValue("id")
	if conversationID == "" {
		http.Error(w, "Missing conversation ID", http.StatusBadRequest)
		return
	}

	var input setChatTitleInput
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&input)
	}
	title := strings.TrimSpace(input.Title)
	if title == "" {
		http.Error(w, "title is required", http.StatusBadRequest)
		return
	}
	// Same cap as the auto-derived title on first send (chatTitleMaxRunes).
	if utf8.RuneCountInString(title) > chatTitleMaxRunes {
		http.Error(w, "title too long", http.StatusBadRequest)
		return
	}

	if h.chatStore == nil {
		http.Error(w, "conversation not found", http.StatusNotFound)
		return
	}

	renamed, err := h.chatStore.SetTitle(r.Context(), conversationID, session.UserID, title)
	if err != nil {
		slog.Error("[Web] chat set title failed", "conversation", conversationID, "err", err)
		http.Error(w, "failed to save title", http.StatusInternalServerError)
		return
	}
	if !renamed {
		http.Error(w, "conversation not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"conversation_id": conversationID, "title": title})
}

// HandleDeleteChatConversation handles DELETE /api/chat/conversations/{id}.
//
// Soft delete, scoped to the owning user; durable across reschedules (no
// resurrection) and does not touch Langfuse telemetry. See the changelog for the
// data-minimization rationale.
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

	cancelled, deleted, err := h.chatStore.SoftDelete(r.Context(), conversationID, session.UserID)
	if err != nil {
		slog.Error("[Web] chat delete conversation failed", "conversation", conversationID, "err", err)
		http.Error(w, "failed to delete conversation", http.StatusInternalServerError)
		return
	}
	// Deleting cancels the conversation's pending interactions; tell the agent so a
	// suspended turn resolves instead of hanging.
	for _, id := range cancelled {
		h.emitRenderableResponse(r.Context(), conversationID, session.UserID, &pb.RenderableResponse{
			Id:     id,
			Action: pb.RenderableAction_RENDERABLE_ACTION_CANCEL,
		})
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

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Error("[Web] chat encode response failed", "err", err)
	}
}
