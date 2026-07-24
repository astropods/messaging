package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
)

func getConversation(t *testing.T, h *Handlers, user, conversationID string) getChatConversationResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/chat/conversations/"+conversationID, nil)
	req.Header.Set("X-User-ID", user)
	req.SetPathValue("id", conversationID)
	w := httptest.NewRecorder()
	h.HandleGetChatConversation(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("get conversation: want 200, got %d body=%q", w.Code, w.Body.String())
	}
	var resp getChatConversationResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp
}

// The conversation fetch surfaces the still-pending interactions (for reload
// re-render), in the client shape, and omits answered ones.
func TestHandleGetChatConversation_PendingInteractions(t *testing.T) {
	h, st := newChatTitleHandlers(t)
	h.interactions = st
	ctx := t.Context()

	if err := st.Upsert(ctx, "conv-1", "user-1", "title"); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	mkRenderable := func(id string) *pb.Renderable {
		return &pb.Renderable{
			Id:             id,
			Kind:           pb.RenderKind_RENDER_KIND_FORM,
			Message:        "pick one",
			DataSchemaJson: `{"type":"object","properties":{"x":{"type":"string"}}}`,
			AllowedActions: []pb.RenderableAction{
				pb.RenderableAction_RENDERABLE_ACTION_SUBMIT,
				pb.RenderableAction_RENDERABLE_ACTION_CANCEL,
			},
		}
	}
	if _, err := st.AppendInteraction(ctx, "conv-1", "user-1", mkRenderable("i1")); err != nil {
		t.Fatalf("append i1: %v", err)
	}
	if _, err := st.AppendInteraction(ctx, "conv-1", "user-1", mkRenderable("i2")); err != nil {
		t.Fatalf("append i2: %v", err)
	}
	// Answer i1 so only i2 remains pending.
	if _, _, err := st.RecordInteractionResponse(ctx, "conv-1", "i1", &pb.RenderableResponse{
		Id: "i1", Action: pb.RenderableAction_RENDERABLE_ACTION_CANCEL,
	}); err != nil {
		t.Fatalf("record i1: %v", err)
	}

	resp := getConversation(t, h, "user-1", "conv-1")
	if len(resp.PendingInteractions) != 1 {
		t.Fatalf("pending: got %d, want 1 (i1 answered)", len(resp.PendingInteractions))
	}
	got := resp.PendingInteractions[0]
	if got.ID != "i2" || got.Kind != "form" {
		t.Errorf("pending item: id=%q kind=%q, want i2/form", got.ID, got.Kind)
	}
	if len(got.Actions) != 2 || got.Actions[0] != "submit" || got.Actions[1] != "cancel" {
		t.Errorf("actions: got %v, want [submit cancel]", got.Actions)
	}
	// dataSchema is re-embedded as a JSON object, not a string.
	var schema map[string]any
	if err := json.Unmarshal(got.DataSchema, &schema); err != nil {
		t.Errorf("dataSchema not an object: %v", err)
	}
}

// With no interaction store wired, the fetch omits pending_interactions entirely.
func TestHandleGetChatConversation_NoInteractionStore(t *testing.T) {
	h, st := newChatTitleHandlers(t)
	if err := st.Upsert(t.Context(), "conv-1", "user-1", "title"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	resp := getConversation(t, h, "user-1", "conv-1")
	if resp.PendingInteractions != nil {
		t.Errorf("want nil pending_interactions without a store, got %v", resp.PendingInteractions)
	}
}
