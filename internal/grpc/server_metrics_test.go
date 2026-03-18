package grpc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/astropods/messaging/internal/metrics"
	"github.com/astropods/messaging/internal/store"
	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
	"github.com/astropods/messaging/pkg/types"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func newMetricsTestServer() *Server {
	threadStore := store.NewThreadHistoryStore(100, 50, time.Hour)
	convStore := store.NewMemoryStore()
	return NewServer(":0", threadStore, convStore, nil)
}

func newMetricsTestMessage(platform string) *pb.Message {
	return &pb.Message{
		Id:             "msg-metrics-1",
		Platform:       platform,
		ConversationId: "conv-metrics-123",
		Content:        "hello",
		Timestamp:      timestamppb.Now(),
		User:           &pb.User{Id: "U123"},
	}
}

func withAgentStream(server *Server) {
	server.streamsMu.Lock()
	server.streams["agent-stream"] = &conversationStream{
		stream:         &captureStream{sendFunc: func(*pb.AgentResponse) error { return nil }},
		conversationID: "agent-stream",
	}
	server.streamsMu.Unlock()
}

// --- MessagesReceived and MessagesForwarded ---

func TestMetrics_MessagesReceivedAndForwarded(t *testing.T) {
	server := newMetricsTestServer()
	withAgentStream(server)

	beforeReceived := testutil.ToFloat64(metrics.MessagesReceived.WithLabelValues("slack"))
	beforeForwarded := testutil.ToFloat64(metrics.MessagesForwarded.WithLabelValues("slack"))

	if err := server.HandleIncomingMessage(context.Background(), newMetricsTestMessage("slack")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := testutil.ToFloat64(metrics.MessagesReceived.WithLabelValues("slack")) - beforeReceived; got != 1 {
		t.Errorf("MessagesReceived: expected +1, got +%v", got)
	}
	if got := testutil.ToFloat64(metrics.MessagesForwarded.WithLabelValues("slack")) - beforeForwarded; got != 1 {
		t.Errorf("MessagesForwarded: expected +1, got +%v", got)
	}
}

// --- MessagesDropped: no_agent ---

func TestMetrics_MessagesDropped_NoAgent(t *testing.T) {
	server := newMetricsTestServer() // no streams

	before := testutil.ToFloat64(metrics.MessagesDropped.WithLabelValues("slack", "no_agent"))

	err := server.HandleIncomingMessage(context.Background(), newMetricsTestMessage("slack"))
	if err == nil {
		t.Fatal("expected error when no agent stream")
	}

	if got := testutil.ToFloat64(metrics.MessagesDropped.WithLabelValues("slack", "no_agent")) - before; got != 1 {
		t.Errorf("MessagesDropped{no_agent}: expected +1, got +%v", got)
	}
}

// --- AgentResponses: by type ---

func TestMetrics_AgentResponses_ByType(t *testing.T) {
	server := newMetricsTestServer()
	adpt := newMockAdapter("slack")
	server.mu.Lock()
	server.adapters["slack"] = adpt
	server.mu.Unlock()

	_ = server.conversationCache.Create(context.Background(), &types.ConversationContext{
		ConversationID: "conv-resp-test",
		Platform:       "slack",
	})

	cases := []struct {
		label    string
		response *pb.AgentResponse
	}{
		{"content", &pb.AgentResponse{
			ConversationId: "conv-resp-test",
			Payload:        &pb.AgentResponse_Content{Content: &pb.ContentChunk{}},
		}},
		{"status", &pb.AgentResponse{
			ConversationId: "conv-resp-test",
			Payload:        &pb.AgentResponse_Status{Status: &pb.StatusUpdate{}},
		}},
		{"error", &pb.AgentResponse{
			ConversationId: "conv-resp-test",
			Payload:        &pb.AgentResponse_Error{Error: &pb.ErrorResponse{}},
		}},
	}

	for _, tc := range cases {
		before := testutil.ToFloat64(metrics.AgentResponses.WithLabelValues(tc.label))
		_ = server.routeAgentResponse(context.Background(), tc.response)
		if got := testutil.ToFloat64(metrics.AgentResponses.WithLabelValues(tc.label)) - before; got != 1 {
			t.Errorf("AgentResponses{%s}: expected +1, got +%v", tc.label, got)
		}
	}
}

// --- RoutingErrors ---

func TestMetrics_RoutingErrors(t *testing.T) {
	server := newMetricsTestServer()
	adpt := newMockAdapter("slack")
	adpt.respErr = errors.New("send failed")
	server.mu.Lock()
	server.adapters["slack"] = adpt
	server.mu.Unlock()

	_ = server.conversationCache.Create(context.Background(), &types.ConversationContext{
		ConversationID: "conv-routing-err",
		Platform:       "slack",
	})

	before := testutil.ToFloat64(metrics.RoutingErrors.WithLabelValues("slack"))

	_ = server.routeAgentResponse(context.Background(), &pb.AgentResponse{
		ConversationId: "conv-routing-err",
		Payload:        &pb.AgentResponse_Content{Content: &pb.ContentChunk{}},
	})

	if got := testutil.ToFloat64(metrics.RoutingErrors.WithLabelValues("slack")) - before; got != 1 {
		t.Errorf("RoutingErrors{slack}: expected +1, got +%v", got)
	}
}
