package web

import (
	"net/http"

	"github.com/astropods/messaging/internal/store"
	"github.com/astropods/messaging/internal/store/sqlite"
	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
)

func protoHistory(h *sqlite.History) *pb.ConversationHistory {
	out := &pb.ConversationHistory{
		IsComplete: h.Complete,
		Messages:   make([]*pb.HistoryMessage, 0, len(h.Messages)),
	}
	for _, m := range h.Messages {
		out.Messages = append(out.Messages, &pb.HistoryMessage{Id: m.ID, Role: m.Role, Content: m.Content})
	}
	return out
}

func replaceThreadHistory(threadStore *store.ThreadHistoryStore, conversationID string, session *Session, h *sqlite.History) {
	threadStore.Clear(conversationID)
	for _, m := range h.Messages {
		user := &pb.User{Id: "agent", Username: "Agent"}
		if m.Role == "user" {
			user = &pb.User{Id: session.UserID, Username: session.Username}
		}
		threadStore.AddMessage(conversationID, &pb.ThreadMessage{MessageId: m.ID, User: user, Content: m.Content})
	}
}

func (h *Handlers) editsSupported() bool {
	if h.chatStore == nil || h.agentConfigStore == nil {
		return false
	}
	return h.agentConfigStore.Get().GetSupportsHistory()
}

func writeEditUnsupported(w http.ResponseWriter) {
	writeJSON(w, http.StatusBadRequest, map[string]string{
		"error":             "edit_unsupported",
		"error_description": "this agent does not support editing messages",
	})
}

func writeTurnInProgress(w http.ResponseWriter) {
	writeJSON(w, http.StatusConflict, map[string]string{
		"error":             "turn_in_progress",
		"error_description": "a response is already in progress on this conversation",
	})
}

func writeInvalidEdit(w http.ResponseWriter) {
	writeJSON(w, http.StatusBadRequest, map[string]string{
		"error":             "invalid_edit",
		"error_description": "edit_of must name a user message in this conversation",
	})
}
