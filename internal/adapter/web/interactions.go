package web

import (
	"context"
	"log/slog"

	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
)

func (a *WebAdapter) handleRenderable(ctx context.Context, conversationID string, r *pb.Renderable) {
	if r == nil {
		return
	}
	a.emitRenderable(ctx, conversationID, r)
}

func (a *WebAdapter) emitRenderable(ctx context.Context, conversationID string, r *pb.Renderable) {
	event, err := NewInteractionEvent(r)
	if err != nil {
		slog.Error("[Web] dropping malformed renderable", "conversation", conversationID, "err", err)
		return
	}

	if a.interactions != nil {
		if _, err := a.interactions.AppendInteraction(ctx, conversationID, a.conversationOwner(ctx, conversationID), r); err != nil {
			slog.Error("[Web] persist interaction failed", "conversation", conversationID, "err", err)
			return
		}
	}

	a.connManager.Broadcast(conversationID, event)
}

// ownsConversation reports whether sender is the authorized responder. It fails
// closed on an unknown (empty) owner: without a known owner, authorization can't
// be proven. Shared by every interaction ownership check so they can't drift.
func ownsConversation(owner, sender string) bool {
	return owner != "" && owner == sender
}

// renderableActionWireName maps an action to the client vocabulary; UNSPECIFIED
// and the system-only UNSUPPORTED map to "" and are never surfaced.
func renderableActionWireName(a pb.RenderableAction) string {
	switch a {
	case pb.RenderableAction_RENDERABLE_ACTION_SUBMIT:
		return "submit"
	case pb.RenderableAction_RENDERABLE_ACTION_DECLINE:
		return "decline"
	case pb.RenderableAction_RENDERABLE_ACTION_CANCEL:
		return "cancel"
	case pb.RenderableAction_RENDERABLE_ACTION_RESPOND:
		return "respond"
	default:
		return ""
	}
}

// renderKindWireName always returns "form": v1 ships only the form strategy.
func renderKindWireName(pb.RenderKind) string {
	return "form"
}
