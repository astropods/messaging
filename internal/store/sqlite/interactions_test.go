package sqlite

import (
	"errors"
	"testing"

	"github.com/astropods/messaging/internal/store"
	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
)

func renderable(id string) *pb.Renderable {
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

func TestInteractions_AppendGetRoundTrip(t *testing.T) {
	st := newTestStore(t)
	ctx := t.Context()

	it, err := st.AppendInteraction(ctx, "conv", "alice", renderable("i1"))
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if it.Seq != 1 || it.Status != store.InteractionPending || it.UserID != "alice" {
		t.Fatalf("append result: seq=%d status=%q user=%q", it.Seq, it.Status, it.UserID)
	}

	got, found, err := st.GetInteraction(ctx, "conv", "i1")
	if err != nil || !found {
		t.Fatalf("get: found=%v err=%v", found, err)
	}
	// Renderable round-trips through render_json.
	if got.Renderable.GetMessage() != "pick one" || got.Renderable.GetDataSchemaJson() == "" {
		t.Errorf("renderable not preserved: %+v", got.Renderable)
	}
	if len(got.Renderable.GetAllowedActions()) != 2 {
		t.Errorf("allowed actions not preserved: %v", got.Renderable.GetAllowedActions())
	}

	if _, found, _ := st.GetInteraction(ctx, "conv", "missing"); found {
		t.Errorf("missing interaction reported found")
	}
}

func TestInteractions_AppendIdempotentByID(t *testing.T) {
	st := newTestStore(t)
	ctx := t.Context()

	if _, err := st.AppendInteraction(ctx, "conv", "alice", renderable("i1")); err != nil {
		t.Fatalf("append: %v", err)
	}
	if _, _, err := st.RecordInteractionResponse(ctx, "conv", "i1", &pb.RenderableResponse{
		Id: "i1", Action: pb.RenderableAction_RENDERABLE_ACTION_CANCEL,
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	// Re-append the same id: returns the existing (answered) record, no duplicate.
	again, err := st.AppendInteraction(ctx, "conv", "alice", renderable("i1"))
	if err != nil {
		t.Fatalf("re-append: %v", err)
	}
	if again.Status != store.InteractionCancelled || again.Seq != 1 {
		t.Errorf("re-append changed record: status=%q seq=%d", again.Status, again.Seq)
	}
	all, _ := st.ListInteractions(ctx, "conv")
	if len(all) != 1 {
		t.Fatalf("want 1 interaction (no duplicate), got %d", len(all))
	}
}

func TestInteractions_RecordIdempotentAndContent(t *testing.T) {
	st := newTestStore(t)
	ctx := t.Context()
	if _, err := st.AppendInteraction(ctx, "conv", "alice", renderable("i1")); err != nil {
		t.Fatalf("append: %v", err)
	}

	resp := &pb.RenderableResponse{
		Id: "i1", Action: pb.RenderableAction_RENDERABLE_ACTION_SUBMIT, ContentJson: `{"x":"y"}`,
	}
	got, recorded, err := st.RecordInteractionResponse(ctx, "conv", "i1", resp)
	if err != nil || !recorded {
		t.Fatalf("first record: recorded=%v err=%v", recorded, err)
	}
	if got.Status != store.InteractionSubmitted || got.RespondedAt.IsZero() {
		t.Fatalf("record result: status=%q respondedAt=%v", got.Status, got.RespondedAt)
	}
	if got.Response.GetContentJson() != `{"x":"y"}` {
		t.Errorf("response content not preserved: %q", got.Response.GetContentJson())
	}

	// Second record is a replay: not recorded, original response intact.
	second, recorded, err := st.RecordInteractionResponse(ctx, "conv", "i1", &pb.RenderableResponse{
		Id: "i1", Action: pb.RenderableAction_RENDERABLE_ACTION_CANCEL,
	})
	if err != nil {
		t.Fatalf("second record: %v", err)
	}
	if recorded {
		t.Error("second record should not re-record")
	}
	if second.Status != store.InteractionSubmitted || second.Response.GetAction() != pb.RenderableAction_RENDERABLE_ACTION_SUBMIT {
		t.Errorf("replay changed the stored outcome: %+v", second)
	}

	// Survives a reload (fresh read).
	reloaded, _, _ := st.GetInteraction(ctx, "conv", "i1")
	if reloaded.Status != store.InteractionSubmitted || reloaded.Response.GetContentJson() != `{"x":"y"}` {
		t.Errorf("not durable: %+v", reloaded)
	}
}

func TestInteractions_RecordMissing(t *testing.T) {
	st := newTestStore(t)
	if _, _, err := st.RecordInteractionResponse(t.Context(), "conv", "nope", &pb.RenderableResponse{Id: "nope"}); !errors.Is(err, store.ErrInteractionNotFound) {
		t.Fatalf("want ErrInteractionNotFound, got %v", err)
	}
}

func TestInteractions_PendingFIFO(t *testing.T) {
	st := newTestStore(t)
	ctx := t.Context()
	for _, id := range []string{"i1", "i2", "i3"} {
		if _, err := st.AppendInteraction(ctx, "conv", "alice", renderable(id)); err != nil {
			t.Fatalf("append %s: %v", id, err)
		}
	}
	if _, _, err := st.RecordInteractionResponse(ctx, "conv", "i2", &pb.RenderableResponse{
		Id: "i2", Action: pb.RenderableAction_RENDERABLE_ACTION_CANCEL,
	}); err != nil {
		t.Fatalf("record: %v", err)
	}

	pending, _ := st.PendingInteractions(ctx, "conv")
	if len(pending) != 2 || pending[0].ID != "i1" || pending[1].ID != "i3" {
		t.Fatalf("pending FIFO wrong: %+v", pending)
	}
}

// Interactions and messages draw from one seq space, so a conversation's combined
// timeline is strictly ordered.
func TestInteractions_SharedSeqInterleaved(t *testing.T) {
	st := newTestStore(t)
	ctx := t.Context()
	if _, err := st.EnsureForSend(ctx, "conv", "alice", "t"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	m1, err := st.AppendMessage(ctx, "conv", "alice", "user", "hello")
	if err != nil {
		t.Fatalf("msg1: %v", err)
	}
	i1, err := st.AppendInteraction(ctx, "conv", "alice", renderable("i1"))
	if err != nil {
		t.Fatalf("int1: %v", err)
	}
	m2, err := st.AppendMessage(ctx, "conv", "alice", "assistant", "hi")
	if err != nil {
		t.Fatalf("msg2: %v", err)
	}
	i2, err := st.AppendInteraction(ctx, "conv", "alice", renderable("i2"))
	if err != nil {
		t.Fatalf("int2: %v", err)
	}

	// One monotonic sequence across both tables, no collisions.
	if m1.Seq != 1 || i1.Seq != 2 || m2.Seq != 3 || i2.Seq != 4 {
		t.Fatalf("shared seq wrong: msg1=%d int1=%d msg2=%d int2=%d", m1.Seq, i1.Seq, m2.Seq, i2.Seq)
	}
}

// Interactions occupy shared seq slots for ordering but must not count against
// the per-conversation message cap (which bounds message rows).
func TestInteractions_DoNotConsumeMessageCap(t *testing.T) {
	st := newTestStore(t)
	ctx := t.Context()
	if _, err := st.EnsureForSend(ctx, "conv", "alice", "t"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	for _, id := range []string{"i1", "i2", "i3"} {
		if _, err := st.AppendInteraction(ctx, "conv", "alice", renderable(id)); err != nil {
			t.Fatalf("append %s: %v", id, err)
		}
	}

	m, err := st.AppendMessage(ctx, "conv", "alice", "user", "hi")
	if err != nil {
		t.Fatalf("append message: %v", err)
	}
	// Shared seq advanced past the interactions, but the cap sees message rows only.
	if m.Seq != 4 {
		t.Errorf("shared seq: got %d, want 4", m.Seq)
	}
	if msgs, _ := st.ListMessages(ctx, "conv"); len(msgs) != 1 {
		t.Errorf("message count: got %d, want 1 (interactions must not count)", len(msgs))
	}
}

// An interaction ahead of the oldest message gives that message a seq > 1; hasMore
// must still be false (no older messages), not a false positive from seq-as-position.
func TestInteractions_PaginationNotFooledByLeadingInteraction(t *testing.T) {
	st := newTestStore(t)
	ctx := t.Context()
	if _, err := st.EnsureForSend(ctx, "conv", "alice", "t"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Interaction takes seq 1; the only message takes seq 2.
	if _, err := st.AppendInteraction(ctx, "conv", "alice", renderable("i1")); err != nil {
		t.Fatalf("append interaction: %v", err)
	}
	if _, err := st.AppendMessage(ctx, "conv", "alice", "user", "hi"); err != nil {
		t.Fatalf("append message: %v", err)
	}

	msgs, hasMore, oldestSeq, _, err := st.PageMessages(ctx, "conv", 50, 0)
	if err != nil {
		t.Fatalf("page: %v", err)
	}
	if len(msgs) != 1 || oldestSeq != 2 {
		t.Fatalf("page: got %d msgs oldestSeq=%d, want 1 msg at seq 2", len(msgs), oldestSeq)
	}
	if hasMore {
		t.Error("hasMore should be false: the leading seq belongs to an interaction, not an older message")
	}
}

func TestInteractions_SoftDeleteCancelsPending(t *testing.T) {
	st := newTestStore(t)
	ctx := t.Context()
	if _, err := st.EnsureForSend(ctx, "conv", "alice", "t"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := st.AppendInteraction(ctx, "conv", "alice", renderable("i1")); err != nil {
		t.Fatalf("append i1: %v", err)
	}
	if _, err := st.AppendInteraction(ctx, "conv", "alice", renderable("i2")); err != nil {
		t.Fatalf("append i2: %v", err)
	}
	// Answer i1 so only i2 is pending at delete time.
	if _, _, err := st.RecordInteractionResponse(ctx, "conv", "i1", &pb.RenderableResponse{
		Id: "i1", Action: pb.RenderableAction_RENDERABLE_ACTION_SUBMIT, ContentJson: `{"x":"y"}`,
	}); err != nil {
		t.Fatalf("record i1: %v", err)
	}

	cancelled, deleted, err := st.SoftDelete(ctx, "conv", "alice")
	if err != nil || !deleted {
		t.Fatalf("soft delete: deleted=%v err=%v", deleted, err)
	}
	if len(cancelled) != 1 || cancelled[0] != "i2" {
		t.Fatalf("want [i2] cancelled, got %v", cancelled)
	}

	// i2 is now cancelled, i1's outcome untouched, nothing left pending.
	i2, _, _ := st.GetInteraction(ctx, "conv", "i2")
	if i2.Status != store.InteractionCancelled {
		t.Errorf("i2 status: got %q, want cancelled", i2.Status)
	}
	i1, _, _ := st.GetInteraction(ctx, "conv", "i1")
	if i1.Status != store.InteractionSubmitted {
		t.Errorf("i1 outcome changed by delete: %q", i1.Status)
	}
	if pending, _ := st.PendingInteractions(ctx, "conv"); len(pending) != 0 {
		t.Errorf("pending after delete: got %d, want 0", len(pending))
	}
}

// Deleting a conversation with no pending interactions cancels nothing.
func TestInteractions_SoftDeleteNoPending(t *testing.T) {
	st := newTestStore(t)
	ctx := t.Context()
	if _, err := st.EnsureForSend(ctx, "conv", "alice", "t"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cancelled, deleted, err := st.SoftDelete(ctx, "conv", "alice")
	if err != nil || !deleted {
		t.Fatalf("soft delete: deleted=%v err=%v", deleted, err)
	}
	if len(cancelled) != 0 {
		t.Errorf("want no cancellations, got %v", cancelled)
	}
}
