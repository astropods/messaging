package grpc

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	pb "github.com/astromode-ai/astro-messaging/pkg/gen/astro/messaging/v1"
	"github.com/astromode-ai/astro-messaging/internal/adapter"
	"github.com/astromode-ai/astro-messaging/internal/store"
	"github.com/astromode-ai/astro-messaging/pkg/types"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// mockAdapter implements adapter.Adapter for testing
type mockAdapter struct {
	platform  string
	mu        sync.Mutex
	healthy   bool
	responses []*pb.AgentResponse
	respErr   error
	handler   adapter.MessageHandler
}

func newMockAdapter(platform string) *mockAdapter {
	return &mockAdapter{
		platform:  platform,
		healthy:   true,
		responses: make([]*pb.AgentResponse, 0),
	}
}

func (m *mockAdapter) Initialize(ctx context.Context, config adapter.Config) error { return nil }
func (m *mockAdapter) Start(ctx context.Context) error                            { return nil }
func (m *mockAdapter) Stop(ctx context.Context) error                             { return nil }
func (m *mockAdapter) GetPlatformName() string                                    { return m.platform }
func (m *mockAdapter) IsHealthy(ctx context.Context) bool                         { return m.healthy }
func (m *mockAdapter) Capabilities() adapter.AdapterCapabilities {
	return adapter.AdapterCapabilities{}
}
func (m *mockAdapter) SetMessageHandler(handler adapter.MessageHandler) {
	m.handler = handler
}
func (m *mockAdapter) HydrateThread(ctx context.Context, conversationID string, s *store.ThreadHistoryStore) error {
	return nil
}

func (m *mockAdapter) HandleAgentResponse(ctx context.Context, response *pb.AgentResponse) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.respErr != nil {
		return m.respErr
	}
	m.responses = append(m.responses, response)
	return nil
}

func (m *mockAdapter) getLastResponse() *pb.AgentResponse {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.responses) == 0 {
		return nil
	}
	return m.responses[len(m.responses)-1]
}

func (m *mockAdapter) getResponseCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.responses)
}

// --- Tests for HandleIncomingMessage ---

func TestHandleIncomingMessage_ForwardsToStream(t *testing.T) {
	threadStore := store.NewThreadHistoryStore(100, 50, time.Hour)
	convStore := store.NewMemoryStore()
	server := NewServer(":0", threadStore, convStore, nil)

	var capturedResponse *pb.AgentResponse
	mockStream := &captureStream{
		sendFunc: func(resp *pb.AgentResponse) error {
			capturedResponse = resp
			return nil
		},
	}

	server.streamsMu.Lock()
	server.streams["agent-stream"] = &conversationStream{
		stream:         mockStream,
		conversationID: "agent-stream",
	}
	server.streamsMu.Unlock()

	msg := &pb.Message{
		Id:             "msg-100",
		Platform:       "slack",
		ConversationId: "conv-slack-123",
		Content:        "Test message",
		Timestamp:      timestamppb.Now(),
		PlatformContext: &pb.PlatformContext{
			MessageId:   "1234567890.123456",
			ChannelId:   "C123456",
			ThreadId:    "1234567890.000001",
			ChannelName: "#general",
			WorkspaceId: "T123456",
		},
		User: &pb.User{
			Id:       "U123456",
			Username: "testuser",
		},
	}

	err := server.HandleIncomingMessage(context.Background(), msg)
	if err != nil {
		t.Fatalf("HandleIncomingMessage failed: %v", err)
	}

	if capturedResponse == nil {
		t.Fatal("expected message to be forwarded via stream")
	}

	if capturedResponse.ConversationId != "conv-slack-123" {
		t.Errorf("ConversationId: expected 'conv-slack-123', got %q", capturedResponse.ConversationId)
	}

	incoming := capturedResponse.GetIncomingMessage()
	if incoming == nil {
		t.Fatal("expected IncomingMessage payload")
	}

	// Verify full PlatformContext fidelity
	pc := incoming.PlatformContext
	if pc.MessageId != "1234567890.123456" {
		t.Errorf("MessageId: expected '1234567890.123456', got %q", pc.MessageId)
	}
	if pc.ChannelId != "C123456" {
		t.Errorf("ChannelId: expected 'C123456', got %q", pc.ChannelId)
	}
	if pc.ThreadId != "1234567890.000001" {
		t.Errorf("ThreadId: expected '1234567890.000001', got %q", pc.ThreadId)
	}
	if pc.ChannelName != "#general" {
		t.Errorf("ChannelName: expected '#general', got %q", pc.ChannelName)
	}
	if pc.WorkspaceId != "T123456" {
		t.Errorf("WorkspaceId: expected 'T123456', got %q", pc.WorkspaceId)
	}
}

func TestHandleIncomingMessage_NoActiveStream(t *testing.T) {
	threadStore := store.NewThreadHistoryStore(100, 50, time.Hour)
	convStore := store.NewMemoryStore()
	server := NewServer(":0", threadStore, convStore, nil)

	msg := &pb.Message{
		Id:       "msg-101",
		Platform: "web",
		Content:  "Hello",
	}

	err := server.HandleIncomingMessage(context.Background(), msg)
	if err == nil {
		t.Fatal("expected error when no active stream")
	}
}

func TestHandleIncomingMessage_StreamSendError(t *testing.T) {
	threadStore := store.NewThreadHistoryStore(100, 50, time.Hour)
	convStore := store.NewMemoryStore()
	server := NewServer(":0", threadStore, convStore, nil)

	mockStream := &captureStream{
		sendFunc: func(resp *pb.AgentResponse) error {
			return fmt.Errorf("stream broken")
		},
	}

	server.streamsMu.Lock()
	server.streams["agent-stream"] = &conversationStream{
		stream:         mockStream,
		conversationID: "agent-stream",
	}
	server.streamsMu.Unlock()

	msg := &pb.Message{
		Id:       "msg-102",
		Platform: "web",
		Content:  "Hello",
	}

	err := server.HandleIncomingMessage(context.Background(), msg)
	if err == nil {
		t.Fatal("expected error when stream send fails")
	}
}

func TestHandleIncomingMessage_PlatformContextPreserved(t *testing.T) {
	threadStore := store.NewThreadHistoryStore(100, 50, time.Hour)
	convStore := store.NewMemoryStore()
	server := NewServer(":0", threadStore, convStore, nil)

	mock := newMockAdapter("web")
	server.RegisterAdapter("web", mock)

	var capturedResponse *pb.AgentResponse
	mockStream := &captureStream{
		sendFunc: func(resp *pb.AgentResponse) error {
			capturedResponse = resp
			return nil
		},
	}

	server.streamsMu.Lock()
	server.streams["agent-stream"] = &conversationStream{
		stream:         mockStream,
		conversationID: "agent-stream",
	}
	server.streamsMu.Unlock()

	msg := &pb.Message{
		Id:             "msg-001",
		Platform:       "web",
		Content:        "Hello agent",
		ConversationId: "conv-abc",
		Timestamp:      timestamppb.Now(),
		PlatformContext: &pb.PlatformContext{
			MessageId: "plat-msg-001",
			ChannelId: "conv-abc",
			ThreadId:  "thread-xyz",
		},
		User: &pb.User{
			Id:       "user-123",
			Username: "testuser",
		},
	}

	err := server.HandleIncomingMessage(context.Background(), msg)
	if err != nil {
		t.Fatalf("HandleIncomingMessage failed: %v", err)
	}

	if capturedResponse == nil {
		t.Fatal("expected response to be forwarded to agent stream")
	}

	incoming := capturedResponse.GetIncomingMessage()
	if incoming == nil {
		t.Fatal("expected IncomingMessage payload in AgentResponse")
	}

	pc := incoming.PlatformContext
	if pc == nil {
		t.Fatal("expected PlatformContext to be non-nil")
	}

	if pc.MessageId != "plat-msg-001" {
		t.Errorf("PlatformContext.MessageId: expected 'plat-msg-001', got %q", pc.MessageId)
	}
	if pc.ChannelId != "conv-abc" {
		t.Errorf("PlatformContext.ChannelId: expected 'conv-abc', got %q", pc.ChannelId)
	}
	if pc.ThreadId != "thread-xyz" {
		t.Errorf("PlatformContext.ThreadId: expected 'thread-xyz', got %q", pc.ThreadId)
	}
}

func TestHandleIncomingMessage_UserFieldsPreserved(t *testing.T) {
	threadStore := store.NewThreadHistoryStore(100, 50, time.Hour)
	convStore := store.NewMemoryStore()
	server := NewServer(":0", threadStore, convStore, nil)

	var capturedResponse *pb.AgentResponse
	mockStream := &captureStream{
		sendFunc: func(resp *pb.AgentResponse) error {
			capturedResponse = resp
			return nil
		},
	}

	server.streamsMu.Lock()
	server.streams["agent-stream"] = &conversationStream{
		stream:         mockStream,
		conversationID: "agent-stream",
	}
	server.streamsMu.Unlock()

	msg := &pb.Message{
		Id:             "msg-003",
		Platform:       "slack",
		Content:        "Hello from Slack",
		ConversationId: "C99999",
		User: &pb.User{
			Id:       "U12345",
			Username: "slackuser",
		},
	}

	_, err := context.Background(), server.HandleIncomingMessage(context.Background(), msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	incoming := capturedResponse.GetIncomingMessage()
	if incoming.User == nil {
		t.Fatal("expected User to be non-nil")
	}
	if incoming.User.Id != "U12345" {
		t.Errorf("User.Id: expected 'U12345', got %q", incoming.User.Id)
	}
	if incoming.User.Username != "slackuser" {
		t.Errorf("User.Username: expected 'slackuser', got %q", incoming.User.Username)
	}
}

// --- Tests for PlatformContext roundtrip ---

func TestPlatformContext_RoundtripWebMessage(t *testing.T) {
	threadStore := store.NewThreadHistoryStore(100, 50, time.Hour)
	convStore := store.NewMemoryStore()
	server := NewServer(":0", threadStore, convStore, nil)

	webAdapter := newMockAdapter("web")
	server.RegisterAdapter("web", webAdapter)

	// Capture what gets sent to the agent
	var agentReceived *pb.AgentResponse
	mockStream := &captureStream{
		sendFunc: func(resp *pb.AgentResponse) error {
			agentReceived = resp
			return nil
		},
	}

	server.streamsMu.Lock()
	server.streams["agent-stream"] = &conversationStream{
		stream:         mockStream,
		conversationID: "agent-stream",
	}
	server.streamsMu.Unlock()

	// Web adapter sends incoming message as pb.Message directly
	incomingMsg := &pb.Message{
		Id:             "msg-web-001",
		Platform:       "web",
		Content:        "What is an API?",
		ConversationId: "conv-web-123",
		Timestamp:      timestamppb.Now(),
		PlatformContext: &pb.PlatformContext{
			MessageId: "msg-web-001",
			ChannelId: "conv-web-123",
		},
		User: &pb.User{
			Id:       "user-42",
			Username: "webuser",
		},
	}

	err := server.HandleIncomingMessage(context.Background(), incomingMsg)
	if err != nil {
		t.Fatalf("HandleIncomingMessage failed: %v", err)
	}

	incoming := agentReceived.GetIncomingMessage()
	if incoming == nil {
		t.Fatal("agent did not receive IncomingMessage")
	}

	receivedPC := incoming.PlatformContext
	if receivedPC == nil {
		t.Fatal("agent received nil PlatformContext")
	}

	// Agent echoes back the same PlatformContext as an AgentResponse
	agentResponse := &pb.AgentResponse{
		ConversationId: incoming.ConversationId,
		ResponseId:     "agent-resp-001",
		Payload: &pb.AgentResponse_Content{
			Content: &pb.ContentChunk{
				Type:    pb.ContentChunk_END,
				Content: "An API is an Application Programming Interface",
			},
		},
	}

	err = server.routeAgentResponse(context.Background(), agentResponse)
	if err != nil {
		t.Fatalf("routeAgentResponse failed: %v", err)
	}

	// Verify the adapter received the response
	resp := webAdapter.getLastResponse()
	if resp == nil {
		t.Fatal("web adapter did not receive the response")
	}

	if resp.ConversationId != "conv-web-123" {
		t.Errorf("ConversationId: expected 'conv-web-123', got %q", resp.ConversationId)
	}
	endContent := resp.GetContent()
	if endContent == nil {
		t.Fatal("expected Content payload")
	}
	if endContent.Content != "An API is an Application Programming Interface" {
		t.Errorf("Content mismatch: got %q", endContent.Content)
	}
}

func TestPlatformContext_RoundtripSlackThreadedMessage(t *testing.T) {
	threadStore := store.NewThreadHistoryStore(100, 50, time.Hour)
	convStore := store.NewMemoryStore()
	server := NewServer(":0", threadStore, convStore, nil)

	slackAdapter := newMockAdapter("slack")
	server.RegisterAdapter("slack", slackAdapter)

	var agentReceived *pb.AgentResponse
	mockStream := &captureStream{
		sendFunc: func(resp *pb.AgentResponse) error {
			agentReceived = resp
			return nil
		},
	}

	server.streamsMu.Lock()
	server.streams["agent-stream"] = &conversationStream{
		stream:         mockStream,
		conversationID: "agent-stream",
	}
	server.streamsMu.Unlock()

	// Slack threaded message as pb.Message directly
	incomingMsg := &pb.Message{
		Id:             "msg-slack-001",
		Platform:       "slack",
		Content:        "Thread reply",
		ConversationId: "C123456-1234567890.000001",
		Timestamp:      timestamppb.Now(),
		PlatformContext: &pb.PlatformContext{
			MessageId: "1234567890.999999",
			ChannelId: "C123456",
			ThreadId:  "1234567890.000001",
		},
		User: &pb.User{
			Id:       "U123456",
			Username: "slackuser",
		},
	}

	err := server.HandleIncomingMessage(context.Background(), incomingMsg)
	if err != nil {
		t.Fatalf("HandleIncomingMessage failed: %v", err)
	}

	incoming := agentReceived.GetIncomingMessage()
	if incoming == nil {
		t.Fatal("agent did not receive IncomingMessage")
	}

	// Agent sends response
	agentResponse := &pb.AgentResponse{
		ConversationId: incoming.ConversationId,
		Payload: &pb.AgentResponse_Content{
			Content: &pb.ContentChunk{
				Type:    pb.ContentChunk_END,
				Content: "Here's my answer",
			},
		},
	}

	err = server.routeAgentResponse(context.Background(), agentResponse)
	if err != nil {
		t.Fatalf("routeAgentResponse failed: %v", err)
	}

	resp := slackAdapter.getLastResponse()
	if resp == nil {
		t.Fatal("slack adapter did not receive the response")
	}

	if resp.ConversationId != "C123456-1234567890.000001" {
		t.Errorf("ConversationId: expected 'C123456-1234567890.000001', got %q", resp.ConversationId)
	}
}

func TestPlatformContext_PlatformDataPreserved(t *testing.T) {
	threadStore := store.NewThreadHistoryStore(100, 50, time.Hour)
	convStore := store.NewMemoryStore()
	server := NewServer(":0", threadStore, convStore, nil)

	var capturedResponse *pb.AgentResponse
	mockStream := &captureStream{
		sendFunc: func(resp *pb.AgentResponse) error {
			capturedResponse = resp
			return nil
		},
	}

	server.streamsMu.Lock()
	server.streams["agent-stream"] = &conversationStream{
		stream:         mockStream,
		conversationID: "agent-stream",
	}
	server.streamsMu.Unlock()

	msg := &pb.Message{
		Id:             "msg-200",
		Platform:       "slack",
		ConversationId: "conv-200",
		Content:        "Test",
		PlatformContext: &pb.PlatformContext{
			MessageId:   "1234567890.123456",
			ChannelId:   "C123456",
			WorkspaceId: "T999",
			PlatformData: map[string]string{
				"team_id":      "T999",
				"bot_id":       "B123",
				"app_id":       "A456",
				"custom_field": "custom_value",
			},
		},
		User: &pb.User{
			Id:       "U123",
			Username: "test",
		},
	}

	err := server.HandleIncomingMessage(context.Background(), msg)
	if err != nil {
		t.Fatalf("HandleIncomingMessage failed: %v", err)
	}

	incoming := capturedResponse.GetIncomingMessage()
	pc := incoming.PlatformContext

	if len(pc.PlatformData) != 4 {
		t.Errorf("expected 4 platform_data entries, got %d", len(pc.PlatformData))
	}
	if pc.PlatformData["team_id"] != "T999" {
		t.Errorf("platform_data[team_id]: expected 'T999', got %q", pc.PlatformData["team_id"])
	}
	if pc.PlatformData["custom_field"] != "custom_value" {
		t.Errorf("platform_data[custom_field]: expected 'custom_value', got %q", pc.PlatformData["custom_field"])
	}
}

// --- Tests for protobuf serialization edge cases ---

func TestProtobufMessage_ConversationIdPreserved(t *testing.T) {
	threadStore := store.NewThreadHistoryStore(100, 50, time.Hour)
	convStore := store.NewMemoryStore()
	server := NewServer(":0", threadStore, convStore, nil)

	var capturedResponse *pb.AgentResponse
	mockStream := &captureStream{
		sendFunc: func(resp *pb.AgentResponse) error {
			capturedResponse = resp
			return nil
		},
	}

	server.streamsMu.Lock()
	server.streams["agent-stream"] = &conversationStream{
		stream:         mockStream,
		conversationID: "agent-stream",
	}
	server.streamsMu.Unlock()

	msg := &pb.Message{
		Id:             "msg-300",
		Platform:       "web",
		ConversationId: "conv-uuid-with-dashes-123",
		Content:        "Test conversation ID preservation",
		PlatformContext: &pb.PlatformContext{
			MessageId: "msg-300",
			ChannelId: "conv-uuid-with-dashes-123",
		},
	}

	err := server.HandleIncomingMessage(context.Background(), msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if capturedResponse.ConversationId != "conv-uuid-with-dashes-123" {
		t.Errorf("ConversationId: expected 'conv-uuid-with-dashes-123', got %q", capturedResponse.ConversationId)
	}

	incoming := capturedResponse.GetIncomingMessage()
	if incoming.ConversationId != "conv-uuid-with-dashes-123" {
		t.Errorf("IncomingMessage.ConversationId: expected 'conv-uuid-with-dashes-123', got %q", incoming.ConversationId)
	}
}

func TestProtobufMessage_TimestampPreserved(t *testing.T) {
	threadStore := store.NewThreadHistoryStore(100, 50, time.Hour)
	convStore := store.NewMemoryStore()
	server := NewServer(":0", threadStore, convStore, nil)

	var capturedResponse *pb.AgentResponse
	mockStream := &captureStream{
		sendFunc: func(resp *pb.AgentResponse) error {
			capturedResponse = resp
			return nil
		},
	}

	server.streamsMu.Lock()
	server.streams["agent-stream"] = &conversationStream{
		stream:         mockStream,
		conversationID: "agent-stream",
	}
	server.streamsMu.Unlock()

	now := time.Now().UTC().Truncate(time.Second)
	msg := &pb.Message{
		Id:             "msg-301",
		Platform:       "web",
		ConversationId: "conv-301",
		Content:        "Test timestamp",
		Timestamp:      timestamppb.New(now),
		PlatformContext: &pb.PlatformContext{
			MessageId: "msg-301",
			ChannelId: "conv-301",
		},
	}

	err := server.HandleIncomingMessage(context.Background(), msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	incoming := capturedResponse.GetIncomingMessage()
	if incoming.Timestamp == nil {
		t.Fatal("expected Timestamp to be preserved")
	}

	receivedTime := incoming.Timestamp.AsTime().UTC().Truncate(time.Second)
	if !receivedTime.Equal(now) {
		t.Errorf("Timestamp: expected %v, got %v", now, receivedTime)
	}
}

func TestProtobufMessage_AttachmentsPreserved(t *testing.T) {
	threadStore := store.NewThreadHistoryStore(100, 50, time.Hour)
	convStore := store.NewMemoryStore()
	server := NewServer(":0", threadStore, convStore, nil)

	var capturedResponse *pb.AgentResponse
	mockStream := &captureStream{
		sendFunc: func(resp *pb.AgentResponse) error {
			capturedResponse = resp
			return nil
		},
	}

	server.streamsMu.Lock()
	server.streams["agent-stream"] = &conversationStream{
		stream:         mockStream,
		conversationID: "agent-stream",
	}
	server.streamsMu.Unlock()

	msg := &pb.Message{
		Id:             "msg-400",
		Platform:       "slack",
		Content:        "Check this file",
		ConversationId: "C123",
		Timestamp:      timestamppb.Now(),
		Attachments: []*pb.Attachment{
			{
				Type:      pb.Attachment_IMAGE,
				Url:       "https://example.com/image.png",
				Filename:  "screenshot.png",
				MimeType:  "image/png",
				SizeBytes: 1024,
			},
		},
	}

	err := server.HandleIncomingMessage(context.Background(), msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	incoming := capturedResponse.GetIncomingMessage()
	if len(incoming.Attachments) != 1 {
		t.Fatalf("expected 1 attachment, got %d", len(incoming.Attachments))
	}

	att := incoming.Attachments[0]
	if att.Url != "https://example.com/image.png" {
		t.Errorf("Attachment.Url: expected 'https://example.com/image.png', got %q", att.Url)
	}
	if att.Filename != "screenshot.png" {
		t.Errorf("Attachment.Filename: expected 'screenshot.png', got %q", att.Filename)
	}
	if att.MimeType != "image/png" {
		t.Errorf("Attachment.MimeType: expected 'image/png', got %q", att.MimeType)
	}
	if att.SizeBytes != 1024 {
		t.Errorf("Attachment.SizeBytes: expected 1024, got %d", att.SizeBytes)
	}
}

// --- Tests for buildConversationID ---

func TestBuildConversationID(t *testing.T) {
	tests := []struct {
		name      string
		platform  string
		channelID string
		threadID  string
		expected  string
	}{
		{
			name:      "with thread",
			platform:  "slack",
			channelID: "C123456",
			threadID:  "1234567890.123456",
			expected:  "slack-C123456-1234567890.123456",
		},
		{
			name:      "without thread",
			platform:  "slack",
			channelID: "C123456",
			threadID:  "",
			expected:  "slack-C123456",
		},
		{
			name:      "web platform",
			platform:  "web",
			channelID: "conv-uuid-123",
			threadID:  "",
			expected:  "web-conv-uuid-123",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := buildConversationID(tt.platform, tt.channelID, tt.threadID)
			if result != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, result)
			}
		})
	}
}

// --- Tests for HealthCheck ---

func TestHealthCheck_AllHealthy(t *testing.T) {
	threadStore := store.NewThreadHistoryStore(100, 50, time.Hour)
	convStore := store.NewMemoryStore()
	server := NewServer(":0", threadStore, convStore, nil)

	mock1 := newMockAdapter("web")
	mock2 := newMockAdapter("slack")
	server.RegisterAdapter("web", mock1)
	server.RegisterAdapter("slack", mock2)

	resp, err := server.HealthCheck(context.Background(), &pb.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("HealthCheck failed: %v", err)
	}

	if resp.Status != pb.HealthCheckResponse_HEALTHY {
		t.Errorf("expected HEALTHY, got %v", resp.Status)
	}
}

func TestHealthCheck_Degraded(t *testing.T) {
	threadStore := store.NewThreadHistoryStore(100, 50, time.Hour)
	convStore := store.NewMemoryStore()
	server := NewServer(":0", threadStore, convStore, nil)

	healthyAdapter := newMockAdapter("web")
	unhealthyAdapter := newMockAdapter("slack")
	unhealthyAdapter.healthy = false

	server.RegisterAdapter("web", healthyAdapter)
	server.RegisterAdapter("slack", unhealthyAdapter)

	resp, err := server.HealthCheck(context.Background(), &pb.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("HealthCheck failed: %v", err)
	}

	if resp.Status != pb.HealthCheckResponse_DEGRADED {
		t.Errorf("expected DEGRADED, got %v", resp.Status)
	}
}

// --- Tests for RegisterAdapter ---

func TestRegisterAdapter(t *testing.T) {
	threadStore := store.NewThreadHistoryStore(100, 50, time.Hour)
	convStore := store.NewMemoryStore()
	server := NewServer(":0", threadStore, convStore, nil)

	mock := newMockAdapter("web")
	server.RegisterAdapter("web", mock)

	server.mu.RLock()
	adpt, exists := server.adapters["web"]
	server.mu.RUnlock()

	if !exists {
		t.Fatal("expected adapter to be registered")
	}
	if adpt.GetPlatformName() != "web" {
		t.Errorf("expected platform 'web', got %q", adpt.GetPlatformName())
	}
}

// --- Tests for SendToAgent ---

func TestSendToAgent_Success(t *testing.T) {
	threadStore := store.NewThreadHistoryStore(100, 50, time.Hour)
	convStore := store.NewMemoryStore()
	server := NewServer(":0", threadStore, convStore, nil)

	var capturedResponse *pb.AgentResponse
	mockStream := &captureStream{
		sendFunc: func(resp *pb.AgentResponse) error {
			capturedResponse = resp
			return nil
		},
	}

	server.streamsMu.Lock()
	server.streams["conv-123"] = &conversationStream{
		stream:         mockStream,
		conversationID: "conv-123",
	}
	server.streamsMu.Unlock()

	resp := &pb.AgentResponse{
		ConversationId: "conv-123",
		Payload: &pb.AgentResponse_Content{
			Content: &pb.ContentChunk{
				Type:    pb.ContentChunk_DELTA,
				Content: "test content",
			},
		},
	}

	err := server.SendToAgent("conv-123", resp)
	if err != nil {
		t.Fatalf("SendToAgent failed: %v", err)
	}

	if capturedResponse == nil {
		t.Fatal("expected response to be sent")
	}
}

func TestSendToAgent_NoStream(t *testing.T) {
	threadStore := store.NewThreadHistoryStore(100, 50, time.Hour)
	convStore := store.NewMemoryStore()
	server := NewServer(":0", threadStore, convStore, nil)

	resp := &pb.AgentResponse{
		ConversationId: "nonexistent",
	}

	err := server.SendToAgent("nonexistent", resp)
	if err == nil {
		t.Fatal("expected error for nonexistent stream")
	}
}

// --- Tests for AgentConfig handling ---

func TestNewServer_WithAgentConfigStore(t *testing.T) {
	threadStore := store.NewThreadHistoryStore(100, 50, time.Hour)
	convStore := store.NewMemoryStore()
	configStore := store.NewAgentConfigStore()
	server := NewServer(":0", threadStore, convStore, configStore)

	if server.agentConfigStore != configStore {
		t.Error("expected agentConfigStore to be set")
	}
}

func TestNewServer_NilAgentConfigStore(t *testing.T) {
	threadStore := store.NewThreadHistoryStore(100, 50, time.Hour)
	convStore := store.NewMemoryStore()
	server := NewServer(":0", threadStore, convStore, nil)

	if server.agentConfigStore != nil {
		t.Error("expected agentConfigStore to be nil")
	}
}

// --- Tests for routeAgentResponse ---

func TestRouteAgentResponse_RoutesViaCacheToCorrectAdapter(t *testing.T) {
	threadStore := store.NewThreadHistoryStore(100, 50, time.Hour)
	convStore := store.NewMemoryStore()
	server := NewServer(":0", threadStore, convStore, nil)

	webAdapter := newMockAdapter("web")
	slackAdapter := newMockAdapter("slack")
	server.RegisterAdapter("web", webAdapter)
	server.RegisterAdapter("slack", slackAdapter)

	ctx := context.Background()
	convStore.Create(ctx, &types.ConversationContext{
		ConversationID: "conv-web-123",
		Platform:       "web",
		ChannelID:      "conv-web-123",
	})

	response := &pb.AgentResponse{
		ConversationId: "conv-web-123",
		Payload: &pb.AgentResponse_Content{
			Content: &pb.ContentChunk{
				Type:    pb.ContentChunk_DELTA,
				Content: "Hello",
			},
		},
	}

	err := server.routeAgentResponse(ctx, response)
	if err != nil {
		t.Fatalf("routeAgentResponse failed: %v", err)
	}

	if webAdapter.getLastResponse() == nil {
		t.Fatal("expected web adapter to receive response")
	}
	if slackAdapter.getLastResponse() != nil {
		t.Error("slack adapter should not receive response for web conversation")
	}
}

func TestRouteAgentResponse_BroadcastsWhenNotInCache(t *testing.T) {
	threadStore := store.NewThreadHistoryStore(100, 50, time.Hour)
	convStore := store.NewMemoryStore()
	server := NewServer(":0", threadStore, convStore, nil)

	webAdapter := newMockAdapter("web")
	slackAdapter := newMockAdapter("slack")
	server.RegisterAdapter("web", webAdapter)
	server.RegisterAdapter("slack", slackAdapter)

	response := &pb.AgentResponse{
		ConversationId: "unknown-conv",
		Payload: &pb.AgentResponse_Status{
			Status: &pb.StatusUpdate{
				Status: pb.StatusUpdate_THINKING,
			},
		},
	}

	err := server.routeAgentResponse(context.Background(), response)
	if err != nil {
		t.Fatalf("routeAgentResponse failed: %v", err)
	}

	if webAdapter.getLastResponse() == nil {
		t.Error("expected web adapter to receive broadcast")
	}
	if slackAdapter.getLastResponse() == nil {
		t.Error("expected slack adapter to receive broadcast")
	}
}

func TestRouteAgentResponse_StatusUpdate(t *testing.T) {
	threadStore := store.NewThreadHistoryStore(100, 50, time.Hour)
	convStore := store.NewMemoryStore()
	server := NewServer(":0", threadStore, convStore, nil)

	webAdapter := newMockAdapter("web")
	server.RegisterAdapter("web", webAdapter)

	ctx := context.Background()
	convStore.Create(ctx, &types.ConversationContext{
		ConversationID: "conv-1",
		Platform:       "web",
		ChannelID:      "conv-1",
	})

	response := &pb.AgentResponse{
		ConversationId: "conv-1",
		Payload: &pb.AgentResponse_Status{
			Status: &pb.StatusUpdate{
				Status:        pb.StatusUpdate_PROCESSING,
				CustomMessage: "Running search_docs",
				Emoji:         "🔧",
			},
		},
	}

	err := server.routeAgentResponse(ctx, response)
	if err != nil {
		t.Fatalf("routeAgentResponse failed: %v", err)
	}

	resp := webAdapter.getLastResponse()
	if resp == nil {
		t.Fatal("expected adapter to receive response")
	}

	status := resp.GetStatus()
	if status == nil {
		t.Fatal("expected Status payload")
	}
	if status.Status != pb.StatusUpdate_PROCESSING {
		t.Errorf("expected PROCESSING, got %v", status.Status)
	}
	if status.CustomMessage != "Running search_docs" {
		t.Errorf("expected 'Running search_docs', got %q", status.CustomMessage)
	}
}

func TestRouteAgentResponse_ContentChunkSequence(t *testing.T) {
	threadStore := store.NewThreadHistoryStore(100, 50, time.Hour)
	convStore := store.NewMemoryStore()
	server := NewServer(":0", threadStore, convStore, nil)

	webAdapter := newMockAdapter("web")
	server.RegisterAdapter("web", webAdapter)

	ctx := context.Background()
	convStore.Create(ctx, &types.ConversationContext{
		ConversationID: "conv-1",
		Platform:       "web",
		ChannelID:      "conv-1",
	})

	chunks := []struct {
		chunkType pb.ContentChunk_ChunkType
		content   string
	}{
		{pb.ContentChunk_START, ""},
		{pb.ContentChunk_DELTA, "Hello "},
		{pb.ContentChunk_DELTA, "world"},
		{pb.ContentChunk_END, "Hello world"},
	}

	for _, c := range chunks {
		resp := &pb.AgentResponse{
			ConversationId: "conv-1",
			Payload: &pb.AgentResponse_Content{
				Content: &pb.ContentChunk{
					Type:    c.chunkType,
					Content: c.content,
				},
			},
		}
		if err := server.routeAgentResponse(ctx, resp); err != nil {
			t.Fatalf("routeAgentResponse failed for %v: %v", c.chunkType, err)
		}
	}

	if webAdapter.getResponseCount() != 4 {
		t.Errorf("expected 4 responses, got %d", webAdapter.getResponseCount())
	}

	last := webAdapter.getLastResponse()
	endContent := last.GetContent()
	if endContent.Type != pb.ContentChunk_END {
		t.Errorf("expected END chunk, got %v", endContent.Type)
	}
	if endContent.Content != "Hello world" {
		t.Errorf("expected 'Hello world', got %q", endContent.Content)
	}
}

func TestRouteAgentResponse_AdapterReturnsError(t *testing.T) {
	threadStore := store.NewThreadHistoryStore(100, 50, time.Hour)
	convStore := store.NewMemoryStore()
	server := NewServer(":0", threadStore, convStore, nil)

	webAdapter := newMockAdapter("web")
	webAdapter.respErr = fmt.Errorf("adapter broken")
	server.RegisterAdapter("web", webAdapter)

	ctx := context.Background()
	convStore.Create(ctx, &types.ConversationContext{
		ConversationID: "conv-err",
		Platform:       "web",
	})

	resp := &pb.AgentResponse{
		ConversationId: "conv-err",
		Payload: &pb.AgentResponse_Content{
			Content: &pb.ContentChunk{
				Type:    pb.ContentChunk_END,
				Content: "test",
			},
		},
	}

	err := server.routeAgentResponse(ctx, resp)
	if err == nil {
		t.Fatal("expected error when adapter returns error")
	}
	if !strings.Contains(err.Error(), "adapter broken") {
		t.Errorf("expected 'adapter broken' in error, got: %v", err)
	}
}

func TestRouteAgentResponse_EmptyConversationID(t *testing.T) {
	threadStore := store.NewThreadHistoryStore(100, 50, time.Hour)
	convStore := store.NewMemoryStore()
	server := NewServer(":0", threadStore, convStore, nil)

	webAdapter := newMockAdapter("web")
	server.RegisterAdapter("web", webAdapter)

	resp := &pb.AgentResponse{
		ConversationId: "",
		Payload: &pb.AgentResponse_Content{
			Content: &pb.ContentChunk{
				Type:    pb.ContentChunk_END,
				Content: "test",
			},
		},
	}

	err := server.routeAgentResponse(context.Background(), resp)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if webAdapter.getResponseCount() != 1 {
		t.Errorf("expected 1 broadcast response, got %d", webAdapter.getResponseCount())
	}
}

// --- Tests for updateConversationCache ---

func TestUpdateConversationCache_CreatesNewEntry(t *testing.T) {
	threadStore := store.NewThreadHistoryStore(100, 50, time.Hour)
	convStore := store.NewMemoryStore()
	server := NewServer(":0", threadStore, convStore, nil)

	msg := &pb.Message{
		Id:             "msg-001",
		Platform:       "slack",
		ConversationId: "C123-thread-ts",
		Content:        "Hello",
		PlatformContext: &pb.PlatformContext{
			ChannelId: "C123",
			ThreadId:  "thread-ts",
		},
		User: &pb.User{
			Id:       "U456",
			Username: "testuser",
		},
	}

	err := server.updateConversationCache(context.Background(), msg)
	if err != nil {
		t.Fatalf("updateConversationCache failed: %v", err)
	}

	conv, err := convStore.Get(context.Background(), "C123-thread-ts")
	if err != nil {
		t.Fatalf("expected conversation to be in cache: %v", err)
	}

	if conv.Platform != "slack" {
		t.Errorf("Platform: expected 'slack', got %q", conv.Platform)
	}
	if conv.ChannelID != "C123" {
		t.Errorf("ChannelID: expected 'C123', got %q", conv.ChannelID)
	}
	if conv.ThreadID != "thread-ts" {
		t.Errorf("ThreadID: expected 'thread-ts', got %q", conv.ThreadID)
	}
	if conv.UserID != "U456" {
		t.Errorf("UserID: expected 'U456', got %q", conv.UserID)
	}
	if conv.MessageCount != 1 {
		t.Errorf("MessageCount: expected 1, got %d", conv.MessageCount)
	}
}

func TestUpdateConversationCache_UpdatesExistingEntry(t *testing.T) {
	threadStore := store.NewThreadHistoryStore(100, 50, time.Hour)
	convStore := store.NewMemoryStore()
	server := NewServer(":0", threadStore, convStore, nil)

	ctx := context.Background()
	convStore.Create(ctx, &types.ConversationContext{
		ConversationID: "conv-1",
		Platform:       "web",
		ChannelID:      "conv-1",
		MessageCount:   5,
	})

	msg := &pb.Message{
		ConversationId: "conv-1",
		Platform:       "web",
		Content:        "New message",
		User:           &pb.User{Id: "u1"},
	}

	err := server.updateConversationCache(ctx, msg)
	if err != nil {
		t.Fatalf("updateConversationCache failed: %v", err)
	}

	conv, _ := convStore.Get(ctx, "conv-1")
	if conv.MessageCount != 6 {
		t.Errorf("MessageCount: expected 6, got %d", conv.MessageCount)
	}
}

func TestUpdateConversationCache_NilPlatformContext(t *testing.T) {
	threadStore := store.NewThreadHistoryStore(100, 50, time.Hour)
	convStore := store.NewMemoryStore()
	server := NewServer(":0", threadStore, convStore, nil)

	msg := &pb.Message{
		ConversationId:  "conv-no-pc",
		Platform:        "web",
		Content:         "No platform context",
		PlatformContext: nil,
		User:            &pb.User{Id: "u1"},
	}

	err := server.updateConversationCache(context.Background(), msg)
	if err != nil {
		t.Fatalf("should not error with nil PlatformContext: %v", err)
	}

	conv, err := convStore.Get(context.Background(), "conv-no-pc")
	if err != nil {
		t.Fatalf("expected conversation to be created: %v", err)
	}
	if conv.ChannelID != "" {
		t.Errorf("ChannelID: expected empty, got %q", conv.ChannelID)
	}
}

func TestRouteAgentResponse_UsesCache_AfterIncomingMessage(t *testing.T) {
	threadStore := store.NewThreadHistoryStore(100, 50, time.Hour)
	convStore := store.NewMemoryStore()
	server := NewServer(":0", threadStore, convStore, nil)

	webAdapter := newMockAdapter("web")
	slackAdapter := newMockAdapter("slack")
	server.RegisterAdapter("web", webAdapter)
	server.RegisterAdapter("slack", slackAdapter)

	mockStream := &captureStream{
		sendFunc: func(resp *pb.AgentResponse) error { return nil },
	}
	server.streamsMu.Lock()
	server.streams["agent-stream"] = &conversationStream{
		stream:         mockStream,
		conversationID: "agent-stream",
	}
	server.streamsMu.Unlock()

	ctx := context.Background()
	incomingMsg := &pb.Message{
		Id:             "msg-1",
		Platform:       "web",
		ConversationId: "conv-web-999",
		Content:        "Hello",
		PlatformContext: &pb.PlatformContext{
			ChannelId: "conv-web-999",
			MessageId: "msg-1",
		},
		User: &pb.User{Id: "u1"},
	}
	server.HandleIncomingMessage(ctx, incomingMsg)

	agentResp := &pb.AgentResponse{
		ConversationId: "conv-web-999",
		Payload: &pb.AgentResponse_Content{
			Content: &pb.ContentChunk{
				Type:    pb.ContentChunk_END,
				Content: "Hi there!",
			},
		},
	}

	err := server.routeAgentResponse(ctx, agentResp)
	if err != nil {
		t.Fatalf("routeAgentResponse failed: %v", err)
	}

	if webAdapter.getLastResponse() == nil {
		t.Fatal("expected web adapter to receive response")
	}
	if slackAdapter.getLastResponse() != nil {
		t.Error("slack adapter should NOT receive response — cache should route to web only")
	}
}

func TestUpdateConversationCache_SecondMessageIncrements(t *testing.T) {
	threadStore := store.NewThreadHistoryStore(100, 50, time.Hour)
	convStore := store.NewMemoryStore()
	server := NewServer(":0", threadStore, convStore, nil)

	ctx := context.Background()
	msg := &pb.Message{
		Id:             "msg-1",
		Platform:       "web",
		ConversationId: "conv-inc",
		Content:        "First",
		User:           &pb.User{Id: "user-1"},
	}

	server.updateConversationCache(ctx, msg)

	msg2 := &pb.Message{
		Id:             "msg-2",
		Platform:       "web",
		ConversationId: "conv-inc",
		Content:        "Second",
		User:           &pb.User{Id: "user-1"},
	}

	server.updateConversationCache(ctx, msg2)

	conv, _ := convStore.Get(ctx, "conv-inc")
	if conv.MessageCount != 2 {
		t.Errorf("expected messageCount 2 after second message, got %d", conv.MessageCount)
	}
}

// --- captureStream mock ---

type captureStream struct {
	pb.AgentMessaging_ProcessConversationServer
	sendFunc func(*pb.AgentResponse) error
}

func (s *captureStream) Send(resp *pb.AgentResponse) error {
	if s.sendFunc != nil {
		return s.sendFunc(resp)
	}
	return nil
}

func (s *captureStream) Context() context.Context {
	return context.Background()
}
