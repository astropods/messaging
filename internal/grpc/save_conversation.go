package grpc

import (
	"context"
	"log/slog"
	"strings"

	"github.com/astropods/messaging/internal/metrics"
	"github.com/astropods/messaging/internal/store/sqlite"
	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// workosUserPrefix is the shape of a resolved Astro identity. Requiring it stops
// an agent addressing a save to a raw Slack id or an arbitrary string, which
// would write a conversation no session can ever open.
const workosUserPrefix = "user_"

var onConflictFromProto = map[pb.SaveConversationRequest_OnConflict]sqlite.OnConflict{
	pb.SaveConversationRequest_ON_CONFLICT_UNSPECIFIED: sqlite.OnConflictSkip,
	pb.SaveConversationRequest_SKIP:                    sqlite.OnConflictSkip,
	pb.SaveConversationRequest_REPLACE:                 sqlite.OnConflictReplace,
	pb.SaveConversationRequest_APPEND:                  sqlite.OnConflictAppend,
}

var statusToProto = map[sqlite.SaveStatus]pb.SaveConversationResponse_Status{
	sqlite.SaveCreated:         pb.SaveConversationResponse_CREATED,
	sqlite.SaveReplaced:        pb.SaveConversationResponse_REPLACED,
	sqlite.SaveAppended:        pb.SaveConversationResponse_APPENDED,
	sqlite.SaveSkippedDeleted:  pb.SaveConversationResponse_SKIPPED_DELETED,
	sqlite.SaveSkippedDiverged: pb.SaveConversationResponse_SKIPPED_DIVERGED,
	sqlite.SaveSkippedConflict: pb.SaveConversationResponse_SKIPPED_CONFLICT,
}

// SaveConversation copies a conversation from another system into the owning
// user's chat history and reports what it did with an existing copy.
func (s *Server) SaveConversation(
	ctx context.Context, req *pb.SaveConversationRequest,
) (*pb.SaveConversationResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	if !strings.HasPrefix(req.UserId, workosUserPrefix) {
		return nil, status.Errorf(codes.InvalidArgument,
			"user_id must be an Astro user id (got %q)", req.UserId)
	}
	if req.IdempotencyKey == "" {
		return nil, status.Error(codes.InvalidArgument, "idempotency_key is required")
	}
	if s.chatStore == nil {
		return nil, status.Error(codes.FailedPrecondition, "chat persistence is disabled")
	}

	msgs := make([]sqlite.SavedMessage, 0, len(req.Messages))
	for _, m := range req.Messages {
		if m.Role != "user" && m.Role != "assistant" {
			return nil, status.Errorf(codes.InvalidArgument,
				"message role must be user or assistant (got %q)", m.Role)
		}
		saved := sqlite.SavedMessage{Role: m.Role, Author: m.Author, Content: m.Content}
		// AsTime() on a nil timestamp is the unix epoch, not the zero value, so
		// the store would stamp created_at at 1970 instead of falling back to now.
		if m.Timestamp != nil {
			saved.Timestamp = m.Timestamp.AsTime()
		}
		msgs = append(msgs, saved)
	}

	convID, saveStatus, err := s.chatStore.SaveConversation(ctx, sqlite.SaveRequest{
		UserID:         req.UserId,
		IdempotencyKey: req.IdempotencyKey,
		Title:          req.Title,
		SourceLabel:    req.SourceLabel,
		SourceURL:      req.SourceUrl,
		Messages:       msgs,
		OnConflict:     onConflictFromProto[req.OnConflict],
	})
	if err != nil {
		slog.Error("[gRPC] SaveConversation failed", "err", err, "user_id", req.UserId)
		return nil, status.Error(codes.Internal, "failed to save conversation")
	}

	metrics.SaveConversations.WithLabelValues(string(saveStatus)).Inc()
	slog.Debug("[gRPC] SaveConversation",
		"conversation", convID, "user_id", req.UserId,
		"status", string(saveStatus), "messages", len(msgs))
	return &pb.SaveConversationResponse{
		ConversationId: convID,
		Status:         statusToProto[saveStatus],
	}, nil
}
