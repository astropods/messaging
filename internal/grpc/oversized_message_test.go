package grpc

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/astropods/messaging/internal/adapter"
	"github.com/astropods/messaging/internal/store"
	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// Runs over real gRPC: a fake stream cannot show that a failed send ends the RPC.
func TestHandleIncomingMessage_OversizedMessageKeepsAgentStream(t *testing.T) {
	server := NewServer("", store.NewThreadHistoryStore(100, 50, time.Hour), store.NewMemoryStore(), nil)

	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer(grpc.MaxRecvMsgSize(maxMessageBytes), grpc.MaxSendMsgSize(maxMessageBytes))
	pb.RegisterAgentMessagingServer(gs, server)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(16<<20)),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	stream, err := pb.NewAgentMessagingClient(conn).ProcessConversation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&pb.ConversationRequest{Request: &pb.ConversationRequest_AgentConfig{AgentConfig: &pb.AgentConfig{}}}); err != nil {
		t.Fatal(err)
	}
	for !server.AgentConnected() {
		if ctx.Err() != nil {
			t.Fatal("agent stream never registered")
		}
		time.Sleep(10 * time.Millisecond)
	}

	big := &pb.Message{Id: "big", Platform: "slack", ConversationId: "C1-1", Content: strings.Repeat("x", maxMessageBytes)}
	if err := server.HandleIncomingMessage(ctx, big); !errors.Is(err, adapter.ErrMessageTooLarge) {
		t.Errorf("oversized message: got %v, want ErrMessageTooLarge", err)
	}

	if err := server.HandleIncomingMessage(ctx, &pb.Message{Id: "small", Platform: "slack", ConversationId: "C1-2", Content: "hi"}); err != nil {
		t.Fatalf("message after the oversized one: %v", err)
	}
	resp, err := stream.Recv()
	if err != nil {
		t.Fatalf("agent stream ended: %v", err)
	}
	if got := resp.GetIncomingMessage().GetId(); got != "small" {
		t.Errorf("agent received %q, want small", got)
	}
}
