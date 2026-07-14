package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/astropods/messaging/internal/store"
	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type interactionResponseInput struct {
	Action  string          `json:"action"`
	Content json.RawMessage `json:"content,omitempty"` // SUBMIT payload
	Text    string          `json:"text,omitempty"`    // RESPOND payload
}

type interactionResponseAck struct {
	Status string `json:"status"`
	Action string `json:"action"`
}

// HandleInteractionResponse handles POST
// /api/chat/conversations/{id}/interactions/{interactionId}: the user's answer to
// a blocking interaction.
func (h *Handlers) HandleInteractionResponse(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	session := h.authenticate(w, r)
	if session == nil {
		return
	}

	conversationID := r.PathValue("id")
	interactionID := r.PathValue("interactionId")
	if conversationID == "" || interactionID == "" {
		http.Error(w, "Missing conversation or interaction ID", http.StatusBadRequest)
		return
	}
	if h.interactions == nil {
		http.Error(w, "interactions unavailable", http.StatusServiceUnavailable)
		return
	}

	// Cap the body: a crafted schema plus a large instance can validate superlinearly.
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var input interactionResponseInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	it, found, err := h.interactions.GetInteraction(ctx, conversationID, interactionID)
	if err != nil {
		slog.Error("[Web] load interaction failed", "conversation", conversationID, "interaction", interactionID, "err", err)
		http.Error(w, "failed to load interaction", http.StatusInternalServerError)
		return
	}
	if !found {
		http.Error(w, "interaction not found", http.StatusNotFound)
		return
	}

	if !ownsConversation(it.UserID, session.UserID) {
		slog.Warn("[Web] interaction response from unauthorized user", "conversation", conversationID, "interaction", interactionID)
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	// Idempotent replay of an already-answered interaction (e.g. re-POST on reload).
	if it.Status != store.InteractionPending {
		writeJSON(w, http.StatusOK, interactionResponseAck{
			Status: string(it.Status),
			Action: renderableActionWireName(it.Response.GetAction()),
		})
		return
	}

	action, ok := parseResponseAction(input.Action)
	if !ok {
		http.Error(w, "invalid action", http.StatusBadRequest)
		return
	}
	// CANCEL is always permitted (the client always renders a dismiss); any other
	// action must be one the Renderable offered.
	if action != pb.RenderableAction_RENDERABLE_ACTION_CANCEL && !renderableOffersAction(it.Renderable, action) {
		http.Error(w, "action not allowed for this interaction", http.StatusBadRequest)
		return
	}

	resp := &pb.RenderableResponse{Id: interactionID, Action: action}
	switch action {
	case pb.RenderableAction_RENDERABLE_ACTION_SUBMIT:
		if len(input.Content) == 0 {
			http.Error(w, "content is required for submit", http.StatusBadRequest)
			return
		}
		sch, err := compileSchema(it.Renderable.GetDataSchemaJson())
		if err != nil {
			slog.Error("[Web] interaction schema compile failed", "conversation", conversationID, "interaction", interactionID, "err", err)
			http.Error(w, "interaction schema invalid", http.StatusInternalServerError)
			return
		}
		if err := validateInstance(sch, input.Content); err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
				"error":  "content does not match schema",
				"detail": err.Error(),
			})
			return
		}
		resp.ContentJson = string(input.Content)
	case pb.RenderableAction_RENDERABLE_ACTION_RESPOND:
		if strings.TrimSpace(input.Text) == "" {
			http.Error(w, "text is required for respond", http.StatusBadRequest)
			return
		}
		resp.Text = input.Text
	}

	recorded, wasRecorded, err := h.interactions.RecordInteractionResponse(ctx, conversationID, interactionID, resp)
	if err != nil {
		if errors.Is(err, store.ErrInteractionNotFound) {
			http.Error(w, "interaction not found", http.StatusNotFound)
			return
		}
		slog.Error("[Web] record interaction response failed", "conversation", conversationID, "interaction", interactionID, "err", err)
		http.Error(w, "failed to record response", http.StatusInternalServerError)
		return
	}
	// Lost the race to a concurrent responder: replay, don't re-deliver.
	if !wasRecorded {
		writeJSON(w, http.StatusOK, interactionResponseAck{
			Status: string(recorded.Status),
			Action: renderableActionWireName(recorded.Response.GetAction()),
		})
		return
	}

	h.emitRenderableResponse(r.Context(), conversationID, session.UserID, resp)
	writeJSON(w, http.StatusOK, interactionResponseAck{
		Status: string(recorded.Status),
		Action: renderableActionWireName(action),
	})
}

// emitRenderableResponse forwards a response to the local agent over the feedback
// channel. Delivery is best effort: with no agent stream the send is dropped, and
// there is no reconnect redelivery yet, so the agent's render() stays unresolved.
func (h *Handlers) emitRenderableResponse(ctx context.Context, conversationID, userID string, resp *pb.RenderableResponse) {
	if h.feedbackHandler == nil {
		slog.Debug("[Web] no feedback handler; dropping renderable response", "conversation", conversationID)
		return
	}
	fb := &pb.PlatformFeedback{
		ConversationId: conversationID,
		Timestamp:      timestamppb.Now(),
		Feedback:       &pb.PlatformFeedback_RenderableResponse{RenderableResponse: resp},
	}
	if userID != "" {
		fb.User = &pb.User{Id: userID}
	}
	if err := h.feedbackHandler(ctx, fb); err != nil {
		slog.Debug("[Web] renderable response not delivered to agent", "conversation", conversationID, "err", err)
	}
}

// resolveDegradedRespond delivers a user message as the RESPOND answer to a
// degraded free-text-tolerant interaction (forms off) instead of starting a new
// turn. It does not persist the interaction, so — unlike the response endpoint —
// there is no cross-restart durability or exactly-once guarantee.
func (h *Handlers) resolveDegradedRespond(ctx context.Context, conversationID string, session *Session, interactionID, text string) {
	// Persist the reply only if the conversation still exists, so a since-deleted
	// one isn't given an orphan row (or resurrected via EnsureForSend).
	if h.chatStore != nil {
		if conv, err := h.chatStore.Get(ctx, conversationID); err == nil && conv != nil {
			if _, err := h.chatStore.AppendMessage(ctx, conversationID, session.UserID, "user", text); err != nil {
				slog.Error("[Web] chat persist degraded respond failed", "conversation", conversationID, "err", err)
			}
		}
	}
	h.emitRenderableResponse(ctx, conversationID, session.UserID, &pb.RenderableResponse{
		Id:     interactionID,
		Action: pb.RenderableAction_RENDERABLE_ACTION_RESPOND,
		Text:   text,
	})
}

func parseResponseAction(s string) (pb.RenderableAction, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "submit":
		return pb.RenderableAction_RENDERABLE_ACTION_SUBMIT, true
	case "decline":
		return pb.RenderableAction_RENDERABLE_ACTION_DECLINE, true
	case "cancel":
		return pb.RenderableAction_RENDERABLE_ACTION_CANCEL, true
	case "respond":
		return pb.RenderableAction_RENDERABLE_ACTION_RESPOND, true
	default:
		// UNSUPPORTED is system-emitted only and cannot be sent by a client.
		return pb.RenderableAction_RENDERABLE_ACTION_UNSPECIFIED, false
	}
}

func renderableOffersAction(r *pb.Renderable, action pb.RenderableAction) bool {
	for _, a := range r.GetAllowedActions() {
		if a == action {
			return true
		}
	}
	return false
}

// noExternalRefLoader refuses every external $ref: interaction schemas are
// agent-authored (untrusted), and the library's default FileLoader would resolve
// a file:// $ref against the sidecar filesystem at compile time.
type noExternalRefLoader struct{}

func (noExternalRefLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("external $ref not allowed: %s", url)
}

func compileSchema(schemaJSON string) (*jsonschema.Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(schemaJSON))
	if err != nil {
		return nil, fmt.Errorf("parse schema: %w", err)
	}
	c := jsonschema.NewCompiler()
	c.UseLoader(noExternalRefLoader{})
	const url = "mem://interaction/schema.json"
	if err := c.AddResource(url, doc); err != nil {
		return nil, fmt.Errorf("add schema: %w", err)
	}
	sch, err := c.Compile(url)
	if err != nil {
		return nil, fmt.Errorf("compile schema: %w", err)
	}
	return sch, nil
}

func validateInstance(sch *jsonschema.Schema, content json.RawMessage) error {
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(content))
	if err != nil {
		return fmt.Errorf("parse content: %w", err)
	}
	return sch.Validate(inst)
}
