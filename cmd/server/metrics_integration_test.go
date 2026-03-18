package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	grpcserver "github.com/astropods/messaging/internal/grpc"
	"github.com/astropods/messaging/internal/store"
	"github.com/astropods/messaging/pkg/client"
	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type integrationServer struct {
	srv        *grpcserver.Server
	grpcAddr   string
	metricsURL string
	shutdown   func()
}

func startIntegrationServer(t *testing.T) *integrationServer {
	t.Helper()

	grpcLis, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("gRPC listen: %v", err)
	}
	metricsLis, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("metrics listen: %v", err)
	}

	threadStore := store.NewThreadHistoryStore(100, 50, time.Hour)
	convStore := store.NewMemoryStore()
	srv := grpcserver.NewServer(grpcLis.Addr().String(), threadStore, convStore, nil)

	ctx, cancel := context.WithCancel(context.Background())
	go srv.StartOnListener(ctx, grpcLis) //nolint:errcheck

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	metricsSrv := &http.Server{
		Handler:     mux,
		ReadTimeout: 5 * time.Second,
		IdleTimeout: 60 * time.Second,
	}
	go metricsSrv.Serve(metricsLis) //nolint:errcheck

	return &integrationServer{
		srv:        srv,
		grpcAddr:   grpcLis.Addr().String(),
		metricsURL: "http://" + metricsLis.Addr().String() + "/metrics",
		shutdown: func() {
			cancel()
			srv.Stop()
			shutCtx, shutCancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer shutCancel()
			metricsSrv.Shutdown(shutCtx) //nolint:errcheck
		},
	}
}

func scrapeMetrics(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("scrape %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read metrics body: %v", err)
	}
	return string(body)
}

// metricLine finds the value of a metric line whose label set starts with name.
func metricLine(body, name string) string {
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, name) && !strings.HasPrefix(line, "#") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				return parts[len(parts)-1]
			}
		}
	}
	return ""
}

// TestMetricsIntegration_MessagesForwarded starts a real gRPC server and metrics
// server, connects a real agent client, sends 3 messages via HandleIncomingMessage,
// then scrapes /metrics to verify the counters match.
func TestMetricsIntegration_MessagesForwarded(t *testing.T) {
	is := startIntegrationServer(t)
	defer is.shutdown()

	// Wait for gRPC server to be ready.
	time.Sleep(50 * time.Millisecond)

	// Connect a real agent and open a conversation stream so messages can be forwarded.
	agentClient, err := client.NewClient(is.grpcAddr)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	defer agentClient.Close()

	ctx, cancelAgent := context.WithCancel(context.Background())
	defer cancelAgent()

	stream, err := agentClient.ProcessConversation(ctx)
	if err != nil {
		t.Fatalf("ProcessConversation: %v", err)
	}
	// Send a registration message so the server's ProcessConversation loop
	// unblocks from its initial Recv and registers the stream.
	if err := stream.SendMessage(&pb.Message{
		Id:             "agent-register",
		ConversationId: "agent-stream",
		Platform:       "slack",
		Timestamp:      timestamppb.Now(),
		User:           &pb.User{Id: "agent"},
	}); err != nil {
		t.Fatalf("stream registration send: %v", err)
	}

	go stream.ReceiveAll(func(*pb.AgentResponse) error { return nil })

	// Give the stream time to register with the server.
	time.Sleep(50 * time.Millisecond)

	// Send 3 platform messages through the server's incoming message handler.
	for i := range 3 {
		msg := &pb.Message{
			Id:             fmt.Sprintf("integ-msg-%d", i),
			Platform:       "slack",
			ConversationId: "conv-integ",
			Content:        fmt.Sprintf("message %d", i),
			Timestamp:      timestamppb.Now(),
			User:           &pb.User{Id: "U123"},
		}
		if err := is.srv.HandleIncomingMessage(context.Background(), msg); err != nil {
			t.Fatalf("HandleIncomingMessage(%d): %v", i, err)
		}
	}

	body := scrapeMetrics(t, is.metricsURL)

	if got := metricLine(body, `messaging_messages_received_total{platform="slack"}`); got != "3" {
		t.Errorf("messages_received_total{slack}: expected 3, got %q", got)
	}
	if got := metricLine(body, `messaging_messages_forwarded_total{platform="slack"}`); got != "3" {
		t.Errorf("messages_forwarded_total{slack}: expected 3, got %q", got)
	}
}

// TestMetricsIntegration_DroppedNoAgent verifies that messages sent with no agent
// connected are counted in messaging_messages_dropped_total on /metrics.
func TestMetricsIntegration_DroppedNoAgent(t *testing.T) {
	is := startIntegrationServer(t)
	defer is.shutdown()

	// No agent connected — HandleIncomingMessage returns ErrNoAgentStream.
	msg := &pb.Message{
		Id:             "drop-integ-1",
		Platform:       "web",
		ConversationId: "conv-drop-integ",
		Content:        "nobody home",
		Timestamp:      timestamppb.Now(),
		User:           &pb.User{Id: "U999"},
	}
	_ = is.srv.HandleIncomingMessage(context.Background(), msg)

	body := scrapeMetrics(t, is.metricsURL)

	if got := metricLine(body, `messaging_messages_dropped_total{platform="web",reason="no_agent"}`); got == "" || got == "0" {
		t.Errorf("messages_dropped_total{web,no_agent}: expected >0, got %q", got)
	}
}
