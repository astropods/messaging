package teams

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
	"github.com/infracloudio/msbotbuilder-go/core/activity"
	"github.com/infracloudio/msbotbuilder-go/schema"
)

// withTimestamp sets Activity.Timestamp, which MsgOptionText never sets;
// omitempty doesn't apply to non-pointer structs, so it otherwise defaults
// to "0001-01-01T00:00:00Z" and Agents Playground drops the reply silently.
func withTimestamp() activity.MsgOption {
	now := time.Now().UTC()
	return func(a *schema.Activity) error {
		a.Timestamp = now
		return nil
	}
}

// HandleAgentResponse routes content chunks and errors to a Teams reply;
// other payload types have no Teams-side surface.
func (a *TeamsAdapter) HandleAgentResponse(ctx context.Context, response *pb.AgentResponse) error {
	if response == nil {
		return fmt.Errorf("nil response")
	}

	switch payload := response.Payload.(type) {
	case *pb.AgentResponse_Content:
		return a.handleContentChunk(ctx, response.ConversationId, payload.Content)
	case *pb.AgentResponse_Error:
		return a.handleError(ctx, response.ConversationId, payload.Error)
	default:
		slog.Debug("[Teams] ignoring response payload with no Teams surface", "type", fmt.Sprintf("%T", payload))
		return nil
	}
}

// Teams (like Slack) has no incremental-update UX, so content is buffered
// until END.
func (a *TeamsAdapter) handleContentChunk(ctx context.Context, conversationID string, chunk *pb.ContentChunk) error {
	if chunk == nil {
		return fmt.Errorf("nil content chunk")
	}
	if conversationID == "" {
		return fmt.Errorf("empty conversation ID")
	}

	switch chunk.Type {
	case pb.ContentChunk_START:
		a.bufferMu.Lock()
		a.contentBuffers[conversationID] = ""
		a.bufferMu.Unlock()
		return nil

	case pb.ContentChunk_DELTA:
		a.bufferMu.Lock()
		a.contentBuffers[conversationID] += chunk.Content
		a.bufferMu.Unlock()
		return nil

	case pb.ContentChunk_END:
		a.bufferMu.Lock()
		fullContent := a.contentBuffers[conversationID]
		delete(a.contentBuffers, conversationID)
		a.bufferMu.Unlock()

		if fullContent == "" {
			slog.Debug("[Teams] skipping empty message", "conversation_id", conversationID)
			return nil
		}
		return a.sendReply(ctx, conversationID, fullContent)

	default:
		return nil
	}
}

// handleError surfaces an agent-side error as a visible reply; otherwise a
// failed turn produces no reply and looks like the message vanished.
func (a *TeamsAdapter) handleError(ctx context.Context, conversationID string, errorResponse *pb.ErrorResponse) error {
	if errorResponse == nil {
		return fmt.Errorf("nil error response")
	}

	errorMessage := fmt.Sprintf("⚠ Error: %s", errorResponse.Message)
	if errorResponse.Code != pb.ErrorResponse_ERROR_CODE_UNSPECIFIED {
		errorMessage += fmt.Sprintf(" (code: %s)", errorResponse.Code.String())
	}

	return a.sendReply(ctx, conversationID, errorMessage)
}

// sendReply is the real send path: it proactively replies to a
// previously-seen conversation via the stored reference, since the agent's
// response arrives later over gRPC, not from the inbound handler's return.
func (a *TeamsAdapter) sendReply(ctx context.Context, conversationID, text string) error {
	a.refMu.Lock()
	ref, ok := a.refs[conversationID]
	a.refMu.Unlock()
	if !ok {
		return fmt.Errorf("teams: no conversation reference for %s", conversationID)
	}

	handler := activity.HandlerFuncs{
		OnMessageFunc: func(turn *activity.TurnContext) (schema.Activity, error) {
			return turn.SendActivity(activity.MsgOptionText(text), withTimestamp())
		},
	}

	if err := a.botAdapter.ProactiveMessage(ctx, ref, handler); err != nil {
		return fmt.Errorf("teams: send reply: %w", err)
	}
	return nil
}
