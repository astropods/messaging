package web

import (
	"context"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/astropods/messaging/internal/adapter"
	messaginggrpc "github.com/astropods/messaging/internal/grpc"
	"github.com/astropods/messaging/internal/store"
	"github.com/astropods/messaging/internal/store/sqlite"
	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
	"github.com/astropods/messaging/pkg/types"
)

// scriptedAgentStream replays a fixed sequence of ConversationRequests to
// ProcessConversation, then returns io.EOF to simulate the agent stream dropping.
type scriptedAgentStream struct {
	pb.AgentMessaging_ProcessConversationServer
	reqs []*pb.ConversationRequest
	i    int
}

func (s *scriptedAgentStream) Recv() (*pb.ConversationRequest, error) {
	if s.i >= len(s.reqs) {
		return nil, io.EOF
	}
	r := s.reqs[s.i]
	s.i++
	return r, nil
}

func (s *scriptedAgentStream) Send(*pb.AgentResponse) error { return nil }
func (s *scriptedAgentStream) Context() context.Context     { return context.Background() }

func hasErrorEvent(ch <-chan SSEEvent) bool {
	for {
		select {
		case ev := <-ch:
			if ev.Event == EventError {
				return true
			}
		default:
			return false
		}
	}
}

// When the agent's gRPC stream drops mid-turn, ProcessConversation's exit must
// finalize the in-flight turn end to end: the SSE client gets a terminal error
// and the store stops deriving assistant_streaming. Exercises the real wiring
// (server disconnect notify -> web adapter -> failTurn), which the direct
// HandleAgentDisconnect test skips.
func TestProcessConversation_AgentDisconnectFinalizesTurn(t *testing.T) {
	ctx := context.Background()
	const conv, user = "conv-grpc-1", "user-1"

	st, err := sqlite.Open(filepath.Join(t.TempDir(), "chat.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.EnsureForSend(ctx, conv, user, "t"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if _, err := st.AppendMessage(ctx, conv, user, "user", "hi", ""); err != nil {
		t.Fatalf("append: %v", err)
	}

	wa := New()
	if err := wa.Initialize(ctx, adapter.Config{}); err != nil {
		t.Fatalf("init: %v", err)
	}
	wa.SetChatStore(st)

	conn := &SSEConnection{
		ID:             "c",
		ConversationID: conv,
		EventChan:      make(chan SSEEvent, 16),
		Done:           make(chan struct{}),
		CreatedAt:      time.Now(),
	}
	wa.connManager.Add(conn)

	convStore := store.NewMemoryStore()
	if err := convStore.Create(ctx, &types.ConversationContext{ConversationID: conv, Platform: "web", ChannelID: conv}); err != nil {
		t.Fatalf("conv create: %v", err)
	}
	server := messaginggrpc.NewServer(":0", store.NewThreadHistoryStore(100, 50, time.Hour), convStore, nil)
	server.RegisterAdapter("web", wa)

	// Register the stream, stream one content chunk (arms the turn), then EOF.
	stream := &scriptedAgentStream{reqs: []*pb.ConversationRequest{
		{Request: &pb.ConversationRequest_Message{Message: &pb.Message{ConversationId: conv}}},
		{Request: &pb.ConversationRequest_AgentResponse{AgentResponse: &pb.AgentResponse{
			ConversationId: conv,
			Payload:        &pb.AgentResponse_Content{Content: &pb.ContentChunk{Type: pb.ContentChunk_START, Content: "partial"}},
		}}},
	}}
	// Returns when Recv yields io.EOF; the deferred disconnect notify finalizes.
	_ = server.ProcessConversation(stream)

	if !hasErrorEvent(conn.EventChan) {
		t.Fatal("expected a terminal error event after the agent stream dropped")
	}
	got, err := st.Get(ctx, conv)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.AssistantStreaming {
		t.Fatal("conversation still derives assistant_streaming after disconnect")
	}
}
