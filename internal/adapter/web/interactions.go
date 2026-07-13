package web

import (
	"context"
	"log/slog"
	"sync"

	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
	"github.com/google/uuid"
)

func (a *WebAdapter) handleRenderable(ctx context.Context, conversationID string, r *pb.Renderable) {
	if r == nil {
		return
	}
	if a.Capabilities().SupportsDeclarativeForms {
		a.emitRenderable(ctx, conversationID, r)
		return
	}
	a.degradeRenderable(ctx, conversationID, r)
}

func (a *WebAdapter) emitRenderable(ctx context.Context, conversationID string, r *pb.Renderable) {
	event, err := NewInteractionEvent(r)
	if err != nil {
		slog.Error("[Web] dropping malformed renderable", "conversation", conversationID, "err", err)
		return
	}

	// Reject a schema that doesn't compile (including a hostile external $ref) at
	// the source, so it never lands in the store or reaches the client.
	if _, err := compileSchema(r.GetDataSchemaJson()); err != nil {
		slog.Error("[Web] dropping renderable with invalid schema", "conversation", conversationID, "err", err)
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

// degradeRenderable applies the failure contract when the surface can't render a
// form: a free-text-tolerant ask becomes a text prompt answered by the user's
// next message, a strict ask returns a typed UNSUPPORTED.
func (a *WebAdapter) degradeRenderable(ctx context.Context, conversationID string, r *pb.Renderable) {
	if renderableAllowsRespond(r) {
		// Arm the RESPOND capture only if a prompt was shown; otherwise the user's
		// next unrelated message would be silently consumed as the answer.
		if a.emitDegradedPrompt(ctx, conversationID, r) && a.degraded != nil {
			// One pending degrade per conversation: cancel the superseded one so its
			// render() doesn't hang.
			if displaced, ok := a.degraded.set(conversationID, r.GetId(), a.conversationOwner(ctx, conversationID)); ok && displaced != r.GetId() && a.handlers != nil {
				a.handlers.emitRenderableResponse(ctx, conversationID, "", &pb.RenderableResponse{
					Id:     displaced,
					Action: pb.RenderableAction_RENDERABLE_ACTION_CANCEL,
				})
			}
		}
		return
	}
	if a.handlers != nil {
		a.handlers.emitRenderableResponse(ctx, conversationID, "", &pb.RenderableResponse{
			Id:     r.GetId(),
			Action: pb.RenderableAction_RENDERABLE_ACTION_UNSUPPORTED,
		})
	}
}

// emitDegradedPrompt renders the prompt as an assistant message and reports
// whether one was shown (false for an empty message).
func (a *WebAdapter) emitDegradedPrompt(ctx context.Context, conversationID string, r *pb.Renderable) bool {
	msg := r.GetMessage()
	if msg == "" {
		return false
	}
	responseID := uuid.NewString()
	a.connManager.Broadcast(conversationID, NewChunkEvent(&pb.ContentChunk{Type: pb.ContentChunk_START}, responseID))
	a.connManager.Broadcast(conversationID, NewChunkEvent(&pb.ContentChunk{Type: pb.ContentChunk_DELTA, Content: msg}, responseID))
	a.connManager.Broadcast(conversationID, NewChunkEvent(&pb.ContentChunk{Type: pb.ContentChunk_END}, responseID))
	a.connManager.Broadcast(conversationID, NewFinishEvent(responseID))

	// Append, not upsert: don't overwrite a reply the agent streamed earlier this turn.
	if a.chatStore != nil {
		if _, err := a.chatStore.AppendAssistantMessage(ctx, conversationID, msg); err != nil {
			slog.Error("[Web] persist degraded prompt failed", "conversation", conversationID, "err", err)
		}
	}
	return true
}

func (a *WebAdapter) conversationOwner(ctx context.Context, conversationID string) string {
	if a.chatStore == nil {
		return ""
	}
	conv, err := a.chatStore.Get(ctx, conversationID)
	if err != nil || conv == nil {
		return ""
	}
	return conv.UserID
}

// ownsConversation reports whether sender is the authorized responder. It fails
// closed on an unknown (empty) owner: without a known owner, authorization can't
// be proven. Shared by every interaction ownership check so they can't drift.
func ownsConversation(owner, sender string) bool {
	return owner != "" && owner == sender
}

func renderableAllowsRespond(r *pb.Renderable) bool {
	for _, a := range r.GetAllowedActions() {
		if a == pb.RenderableAction_RENDERABLE_ACTION_RESPOND {
			return true
		}
	}
	return false
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

// degradeTracker holds, per conversation, the one free-text-tolerant Renderable
// degraded to a text prompt while forms are off. Bounded like turnTracker.
type degradeTracker struct {
	mu      sync.Mutex
	pending map[string]degradeEntry
}

type degradeEntry struct {
	renderableID string
	owner        string // "" when unknown (no chat store)
}

func newDegradeTracker() *degradeTracker {
	return &degradeTracker{pending: make(map[string]degradeEntry)}
}

// set arms the pending degrade and returns any renderable id it displaced.
func (d *degradeTracker) set(conversationID, renderableID, owner string) (displaced string, hadDisplaced bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	prev, existed := d.pending[conversationID]
	if !existed && len(d.pending) >= maxTrackedStops {
		for k := range d.pending {
			delete(d.pending, k)
			break
		}
	}
	d.pending[conversationID] = degradeEntry{renderableID: renderableID, owner: owner}
	if existed {
		return prev.renderableID, true
	}
	return "", false
}

// take consumes the pending degrade only for its authorized responder; any other
// sender (including when the owner is unknown) leaves the entry in place.
func (d *degradeTracker) take(conversationID, senderID string) (string, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	e, ok := d.pending[conversationID]
	if !ok {
		return "", false
	}
	if !ownsConversation(e.owner, senderID) {
		return "", false
	}
	delete(d.pending, conversationID)
	return e.renderableID, true
}
