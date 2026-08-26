package grpc

import (
	"context"
	"strings"

	"github.com/astropods/messaging/internal/logctx"
	"github.com/astropods/messaging/internal/store/sqlite"
	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
)

// workosUserPrefix is the shape of a resolved Astro identity. Requiring it stops
// an agent addressing a save to a raw Slack id or an arbitrary string, which
// would write a conversation no session can ever open.
const workosUserPrefix = "user_"

// handleSaveConversation writes an external conversation an agent copied in.
// Failures are logged and swallowed: a bad save must not tear down the agent's
// stream and take every in-flight turn with it.
func (s *Server) handleSaveConversation(ctx context.Context, save *pb.SaveConversation) error {
	log := logctx.FromContext(ctx)
	if save == nil {
		return nil
	}
	if s.chatStore == nil {
		log.Debug("[gRPC] SaveConversation ignored; chat persistence disabled")
		return nil
	}
	if !strings.HasPrefix(save.UserId, workosUserPrefix) {
		log.Warn("[gRPC] SaveConversation rejected: user_id is not an Astro user id", "user_id", save.UserId)
		return nil
	}
	if save.IdempotencyKey == "" {
		log.Warn("[gRPC] SaveConversation rejected: empty idempotency_key", "user_id", save.UserId)
		return nil
	}

	msgs := make([]sqlite.SavedMessage, 0, len(save.Messages))
	for _, m := range save.Messages {
		role := m.Role
		if role != "user" && role != "assistant" {
			log.Warn("[gRPC] SaveConversation skipped a message with an unknown role", "role", role)
			continue
		}
		saved := sqlite.SavedMessage{Role: role, Author: m.Author, Content: m.Content}
		// AsTime() on a nil timestamp is the unix epoch, not the zero value, so
		// the store would stamp created_at at 1970 instead of falling back to now.
		if m.Timestamp != nil {
			saved.Timestamp = m.Timestamp.AsTime()
		}
		msgs = append(msgs, saved)
	}

	convID, saved, err := s.chatStore.SaveConversation(
		ctx, save.UserId, save.IdempotencyKey, save.Title, save.SourceLabel, save.SourceUrl, msgs,
	)
	if err != nil {
		log.Error("[gRPC] SaveConversation failed", "err", err, "user_id", save.UserId)
		return nil
	}
	if !saved {
		log.Debug("[gRPC] SaveConversation skipped; the user deleted this copy",
			"conversation", convID, "user_id", save.UserId)
		return nil
	}
	log.Debug("[gRPC] SaveConversation stored",
		"conversation", convID, "user_id", save.UserId, "messages", len(msgs))
	return nil
}
