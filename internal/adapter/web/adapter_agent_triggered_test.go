package web

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/astropods/messaging/internal/store/sqlite"
	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
)

func newAgentTriggeredAdapter(t *testing.T) (*WebAdapter, *sqlite.Store) {
	t.Helper()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "chat.db"))
	if err != nil {
		t.Fatalf("open sqlite store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	return &WebAdapter{
		connManager: NewConnectionManager(30 * time.Second),
		turns:       newTurnTracker(),
		chatStore:   st,
	}, st
}

func content(conv string, kind pb.ContentChunk_ChunkType, text string) *pb.AgentResponse {
	return &pb.AgentResponse{
		ConversationId: conv,
		Payload: &pb.AgentResponse_Content{
			Content: &pb.ContentChunk{Type: kind, Content: text},
		},
	}
}

// Wiring guard for the store's BeginAssistantMessage: HandleAgentResponse must open a
// row on START, or an agent-triggered message (schedule fire, background job) lands on
// the trailing assistant row and silently replaces the previous reply.
func TestWebAdapter_AgentTriggeredMessage_AppendsRatherThanOverwrites(t *testing.T) {
	a, st := newAgentTriggeredAdapter(t)
	ctx := context.Background()
	const conv, user = "conv-1", "user-1"

	if _, err := st.EnsureForSend(ctx, conv, user, "t"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if _, err := st.AppendMessage(ctx, conv, user, "user", "hi", ""); err != nil {
		t.Fatalf("append user: %v", err)
	}
	// A completed normal turn: START then END, the shape the bridge streams.
	for _, r := range []*pb.AgentResponse{
		content(conv, pb.ContentChunk_START, ""),
		content(conv, pb.ContentChunk_DELTA, "the reply"),
		content(conv, pb.ContentChunk_END, ""),
	} {
		if err := a.HandleAgentResponse(ctx, r); err != nil {
			t.Fatalf("normal turn: %v", err)
		}
	}

	// Now an agent-triggered message, with no user turn ahead of it.
	for _, r := range []*pb.AgentResponse{
		content(conv, pb.ContentChunk_START, ""),
		content(conv, pb.ContentChunk_END, "scheduled update"),
	} {
		if err := a.HandleAgentResponse(ctx, r); err != nil {
			t.Fatalf("agent-triggered: %v", err)
		}
	}

	msgs, err := st.ListMessages(ctx, conv)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("want 3 rows (user, reply, pushed), got %d: %+v", len(msgs), msgs)
	}
	if msgs[1].Content != "the reply" {
		t.Fatalf("the agent-triggered message overwrote the turn's reply: %q", msgs[1].Content)
	}
	if msgs[2].Content != "scheduled update" {
		t.Fatalf("pushed message not persisted: %q", msgs[2].Content)
	}
}

// The inverse guard: a normal streamed turn must not gain an extra empty row from the
// START hook, which would show as a blank bubble before every reply.
func TestWebAdapter_NormalTurn_ProducesOneAssistantRow(t *testing.T) {
	a, st := newAgentTriggeredAdapter(t)
	ctx := context.Background()
	const conv, user = "conv-1", "user-1"

	if _, err := st.EnsureForSend(ctx, conv, user, "t"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if _, err := st.AppendMessage(ctx, conv, user, "user", "hi", ""); err != nil {
		t.Fatalf("append user: %v", err)
	}
	for _, r := range []*pb.AgentResponse{
		content(conv, pb.ContentChunk_START, ""),
		content(conv, pb.ContentChunk_DELTA, "part"),
		content(conv, pb.ContentChunk_DELTA, " and rest"),
		content(conv, pb.ContentChunk_END, ""),
	} {
		if err := a.HandleAgentResponse(ctx, r); err != nil {
			t.Fatalf("normal turn: %v", err)
		}
	}

	msgs, _ := st.ListMessages(ctx, conv)
	if len(msgs) != 2 || msgs[1].Content != "part and rest" {
		t.Fatalf("normal turn changed shape: %+v", msgs)
	}
}
