package store

import (
	"context"
	"errors"
	"testing"

	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
)

func renderable(id string) *pb.Renderable {
	return &pb.Renderable{
		Id:             id,
		Kind:           pb.RenderKind_RENDER_KIND_FORM,
		Message:        "pick one",
		DataSchemaJson: `{"type":"object"}`,
		AllowedActions: []pb.RenderableAction{
			pb.RenderableAction_RENDERABLE_ACTION_SUBMIT,
			pb.RenderableAction_RENDERABLE_ACTION_CANCEL,
		},
	}
}

func TestMemoryInteractionStore_AppendAssignsSeq(t *testing.T) {
	m := NewMemoryInteractionStore()

	a, err := m.AppendInteraction(context.Background(), "conv", "alice", renderable("i1"))
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	b, err := m.AppendInteraction(context.Background(), "conv", "alice", renderable("i2"))
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if a.Seq != 1 || b.Seq != 2 {
		t.Fatalf("seq: got %d, %d; want 1, 2", a.Seq, b.Seq)
	}
	if a.Status != InteractionPending {
		t.Errorf("status: got %q, want pending", a.Status)
	}
	if a.UserID != "alice" {
		t.Errorf("userID: got %q, want alice", a.UserID)
	}

	// A different conversation has its own seq space.
	c, err := m.AppendInteraction(context.Background(), "other", "bob", renderable("i3"))
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if c.Seq != 1 {
		t.Errorf("seq for new conversation: got %d, want 1", c.Seq)
	}
}

func TestMemoryInteractionStore_AppendIdempotentByID(t *testing.T) {
	m := NewMemoryInteractionStore()
	if _, err := m.AppendInteraction(context.Background(), "conv", "alice", renderable("i1")); err != nil {
		t.Fatalf("append: %v", err)
	}
	// Answer it, then re-append the same id (e.g. an agent re-emit).
	if _, _, err := m.RecordInteractionResponse(context.Background(), "conv", "i1", &pb.RenderableResponse{
		Id:     "i1",
		Action: pb.RenderableAction_RENDERABLE_ACTION_CANCEL,
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	again, err := m.AppendInteraction(context.Background(), "conv", "alice", renderable("i1"))
	if err != nil {
		t.Fatalf("re-append: %v", err)
	}
	// The answered interaction is preserved, not reset to pending.
	if again.Status != InteractionCancelled {
		t.Errorf("re-append reset status: got %q, want cancelled", again.Status)
	}
	if again.Seq != 1 {
		t.Errorf("re-append changed seq: got %d, want 1", again.Seq)
	}
	// No duplicate in the ordered list / FIFO.
	all, _ := m.ListInteractions(context.Background(), "conv")
	if len(all) != 1 {
		t.Fatalf("list: got %d entries, want 1 (no duplicate)", len(all))
	}
}

func TestMemoryInteractionStore_GetFoundAndMissing(t *testing.T) {
	m := NewMemoryInteractionStore()
	if _, err := m.AppendInteraction(context.Background(), "conv", "alice", renderable("i1")); err != nil {
		t.Fatalf("append: %v", err)
	}

	got, found, err := m.GetInteraction(context.Background(), "conv", "i1")
	if err != nil || !found {
		t.Fatalf("get i1: found=%v err=%v", found, err)
	}
	if got.ID != "i1" {
		t.Errorf("id: got %q, want i1", got.ID)
	}

	if _, found, _ := m.GetInteraction(context.Background(), "conv", "missing"); found {
		t.Errorf("missing interaction reported as found")
	}
	if _, found, _ := m.GetInteraction(context.Background(), "nope", "i1"); found {
		t.Errorf("interaction found in wrong conversation")
	}
}

func TestMemoryInteractionStore_RecordIsIdempotent(t *testing.T) {
	m := NewMemoryInteractionStore()
	if _, err := m.AppendInteraction(context.Background(), "conv", "alice", renderable("i1")); err != nil {
		t.Fatalf("append: %v", err)
	}

	resp := &pb.RenderableResponse{
		Id:          "i1",
		Action:      pb.RenderableAction_RENDERABLE_ACTION_SUBMIT,
		ContentJson: `{"ok":true}`,
	}
	got, recorded, err := m.RecordInteractionResponse(context.Background(), "conv", "i1", resp)
	if err != nil || !recorded {
		t.Fatalf("first record: recorded=%v err=%v", recorded, err)
	}
	if got.Status != InteractionSubmitted {
		t.Errorf("status: got %q, want submitted", got.Status)
	}
	if got.RespondedAt.IsZero() {
		t.Errorf("respondedAt not set")
	}

	// Second record is a no-op replay: recorded=false, original response intact.
	second, recorded, err := m.RecordInteractionResponse(context.Background(), "conv", "i1", &pb.RenderableResponse{
		Id:     "i1",
		Action: pb.RenderableAction_RENDERABLE_ACTION_CANCEL,
	})
	if err != nil {
		t.Fatalf("second record: %v", err)
	}
	if recorded {
		t.Errorf("second record should not re-record")
	}
	if second.Status != InteractionSubmitted {
		t.Errorf("status changed on replay: got %q, want submitted", second.Status)
	}
	if second.Response.GetAction() != pb.RenderableAction_RENDERABLE_ACTION_SUBMIT {
		t.Errorf("response overwritten on replay")
	}
}

func TestMemoryInteractionStore_RecordMissing(t *testing.T) {
	m := NewMemoryInteractionStore()
	_, _, err := m.RecordInteractionResponse(context.Background(), "conv", "nope", &pb.RenderableResponse{Id: "nope"})
	if !errors.Is(err, ErrInteractionNotFound) {
		t.Fatalf("want ErrInteractionNotFound, got %v", err)
	}
}

func TestMemoryInteractionStore_ListAndPending(t *testing.T) {
	m := NewMemoryInteractionStore()
	for _, id := range []string{"i1", "i2", "i3"} {
		if _, err := m.AppendInteraction(context.Background(), "conv", "alice", renderable(id)); err != nil {
			t.Fatalf("append %s: %v", id, err)
		}
	}
	// Answer the middle one.
	if _, _, err := m.RecordInteractionResponse(context.Background(), "conv", "i2", &pb.RenderableResponse{
		Id:     "i2",
		Action: pb.RenderableAction_RENDERABLE_ACTION_CANCEL,
	}); err != nil {
		t.Fatalf("record: %v", err)
	}

	all, _ := m.ListInteractions(context.Background(), "conv")
	if len(all) != 3 {
		t.Fatalf("list: got %d, want 3", len(all))
	}
	if all[0].ID != "i1" || all[1].ID != "i2" || all[2].ID != "i3" {
		t.Errorf("list order wrong: %q %q %q", all[0].ID, all[1].ID, all[2].ID)
	}

	pending, _ := m.PendingInteractions(context.Background(), "conv")
	if len(pending) != 2 {
		t.Fatalf("pending: got %d, want 2", len(pending))
	}
	if pending[0].ID != "i1" || pending[1].ID != "i3" {
		t.Errorf("pending FIFO wrong: %q %q", pending[0].ID, pending[1].ID)
	}
}
