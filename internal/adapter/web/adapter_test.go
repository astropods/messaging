package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	pb "github.com/astromode-ai/astro-messaging/pkg/gen/astro/messaging/v1"
	"github.com/astromode-ai/astro-messaging/internal/store"
)

func TestConnectionManager(t *testing.T) {
	cm := NewConnectionManager(30 * time.Second)

	// Test Add
	conn := &SSEConnection{
		ID:             "conn-1",
		ConversationID: "conv-1",
		EventChan:      make(chan SSEEvent, 10),
		Done:           make(chan struct{}),
		CreatedAt:      time.Now(),
	}
	cm.Add(conn)

	if cm.GetConnectionCount("conv-1") != 1 {
		t.Errorf("expected 1 connection, got %d", cm.GetConnectionCount("conv-1"))
	}

	if cm.GetTotalConnections() != 1 {
		t.Errorf("expected 1 total connection, got %d", cm.GetTotalConnections())
	}

	// Test Broadcast
	event := SSEEvent{Event: "test", Data: `{"foo":"bar"}`}
	cm.Broadcast("conv-1", event)

	select {
	case received := <-conn.EventChan:
		if received.Event != "test" {
			t.Errorf("expected event type 'test', got %s", received.Event)
		}
	case <-time.After(time.Second):
		t.Error("timeout waiting for broadcast event")
	}

	// Test Remove
	cm.Remove("conv-1", "conn-1")

	if cm.GetConnectionCount("conv-1") != 0 {
		t.Errorf("expected 0 connections after remove, got %d", cm.GetConnectionCount("conv-1"))
	}
}

func TestConnectionManager_MultipleConnections(t *testing.T) {
	cm := NewConnectionManager(30 * time.Second)

	// Add multiple connections to same conversation and keep refs
	conns := make([]*SSEConnection, 3)
	for i := 0; i < 3; i++ {
		conns[i] = &SSEConnection{
			ID:             "conn-" + string(rune('1'+i)),
			ConversationID: "conv-1",
			EventChan:      make(chan SSEEvent, 10),
			Done:           make(chan struct{}),
			CreatedAt:      time.Now(),
		}
		cm.Add(conns[i])
	}

	if cm.GetConnectionCount("conv-1") != 3 {
		t.Errorf("expected 3 connections, got %d", cm.GetConnectionCount("conv-1"))
	}

	// Broadcast should reach all connections
	cm.Broadcast("conv-1", SSEEvent{Event: "test", Data: "{}"})

	// Verify all connections received the event
	for i, conn := range conns {
		select {
		case event := <-conn.EventChan:
			if event.Event != "test" {
				t.Errorf("connection %d: expected event 'test', got %s", i, event.Event)
			}
		case <-time.After(100 * time.Millisecond):
			t.Errorf("connection %d: timeout waiting for event", i)
		}
	}
}

func TestConnectionManager_BroadcastNonExistent(t *testing.T) {
	cm := NewConnectionManager(30 * time.Second)

	// Broadcast to non-existent conversation should not panic
	cm.Broadcast("nonexistent", SSEEvent{Event: "test", Data: "{}"})
}

func TestConnectionManager_CloseAll(t *testing.T) {
	cm := NewConnectionManager(30 * time.Second)

	conn1 := &SSEConnection{
		ID:             "conn-1",
		ConversationID: "conv-1",
		EventChan:      make(chan SSEEvent, 10),
		Done:           make(chan struct{}),
	}
	conn2 := &SSEConnection{
		ID:             "conn-2",
		ConversationID: "conv-2",
		EventChan:      make(chan SSEEvent, 10),
		Done:           make(chan struct{}),
	}
	cm.Add(conn1)
	cm.Add(conn2)

	if cm.GetTotalConnections() != 2 {
		t.Errorf("expected 2 connections, got %d", cm.GetTotalConnections())
	}

	cm.CloseAll()

	if cm.GetTotalConnections() != 0 {
		t.Errorf("expected 0 connections after CloseAll, got %d", cm.GetTotalConnections())
	}

	// Verify Done channels are closed
	select {
	case <-conn1.Done:
		// Expected
	default:
		t.Error("conn1.Done should be closed")
	}
	select {
	case <-conn2.Done:
		// Expected
	default:
		t.Error("conn2.Done should be closed")
	}
}

func TestSSEEventFormat(t *testing.T) {
	tests := []struct {
		name     string
		event    SSEEvent
		expected string
	}{
		{
			name:     "simple event",
			event:    SSEEvent{Event: "chunk", Data: `{"content":"hello"}`},
			expected: "event: chunk\ndata: {\"content\":\"hello\"}\n\n",
		},
		{
			name:     "event with ID",
			event:    SSEEvent{Event: "chunk", ID: "123", Data: `{}`},
			expected: "id: 123\nevent: chunk\ndata: {}\n\n",
		},
		{
			name:     "event with retry",
			event:    SSEEvent{Event: "error", Data: `{}`, Retry: 5000},
			expected: "event: error\nretry: 5000\ndata: {}\n\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.event.Format()
			if result != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, result)
			}
		})
	}
}

func TestNewChunkEvent(t *testing.T) {
	tests := []struct {
		name          string
		chunkType     pb.ContentChunk_ChunkType
		expectedType  string
	}{
		{"start", pb.ContentChunk_START, "start"},
		{"delta", pb.ContentChunk_DELTA, "delta"},
		{"end", pb.ContentChunk_END, "end"},
		{"replace", pb.ContentChunk_REPLACE, "replace"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chunk := &pb.ContentChunk{
				Type:              tt.chunkType,
				Content:           "Hello world",
				PlatformMessageId: "msg-123",
			}

			event := NewChunkEvent(chunk, "resp-1")

			if event.Event != EventChunk {
				t.Errorf("expected event type %s, got %s", EventChunk, event.Event)
			}

			var data ChunkEventData
			if err := json.Unmarshal([]byte(event.Data), &data); err != nil {
				t.Fatalf("failed to unmarshal event data: %v", err)
			}

			if data.Content != "Hello world" {
				t.Errorf("expected content 'Hello world', got %s", data.Content)
			}
			if data.ChunkType != tt.expectedType {
				t.Errorf("expected chunk type %q, got %q", tt.expectedType, data.ChunkType)
			}
			if data.ResponseID != "resp-1" {
				t.Errorf("expected response ID 'resp-1', got %s", data.ResponseID)
			}
		})
	}
}

func TestNewConnectedEvent(t *testing.T) {
	event := NewConnectedEvent("conv-123", "conn-456")

	if event.Event != EventConnected {
		t.Errorf("expected event type %s, got %s", EventConnected, event.Event)
	}

	var data ConnectedEventData
	if err := json.Unmarshal([]byte(event.Data), &data); err != nil {
		t.Fatalf("failed to unmarshal event data: %v", err)
	}

	if data.ConversationID != "conv-123" {
		t.Errorf("expected conversation ID 'conv-123', got %s", data.ConversationID)
	}
	if data.ConnectionID != "conn-456" {
		t.Errorf("expected connection ID 'conn-456', got %s", data.ConnectionID)
	}
}

func TestNewFinishEvent(t *testing.T) {
	event := NewFinishEvent("resp-123")

	if event.Event != EventFinish {
		t.Errorf("expected event type %s, got %s", EventFinish, event.Event)
	}

	var data FinishEventData
	if err := json.Unmarshal([]byte(event.Data), &data); err != nil {
		t.Fatalf("failed to unmarshal event data: %v", err)
	}

	if data.ResponseID != "resp-123" {
		t.Errorf("expected response ID 'resp-123', got %s", data.ResponseID)
	}
}

func TestNewPromptsEvent(t *testing.T) {
	prompts := &pb.SuggestedPrompts{
		Prompts: []*pb.SuggestedPrompts_Prompt{
			{Id: "p1", Title: "Help", Message: "How can I help?", Description: "Get assistance"},
			{Id: "p2", Title: "Status", Message: "What is my status?"},
		},
	}

	event := NewPromptsEvent(prompts)

	if event.Event != EventPrompts {
		t.Errorf("expected event type %s, got %s", EventPrompts, event.Event)
	}

	var data PromptsEventData
	if err := json.Unmarshal([]byte(event.Data), &data); err != nil {
		t.Fatalf("failed to unmarshal event data: %v", err)
	}

	if len(data.Prompts) != 2 {
		t.Fatalf("expected 2 prompts, got %d", len(data.Prompts))
	}
	if data.Prompts[0].Title != "Help" {
		t.Errorf("expected first prompt title 'Help', got %s", data.Prompts[0].Title)
	}
}

func TestNewErrorEventFromMessage(t *testing.T) {
	event := NewErrorEventFromMessage("INTERNAL_ERROR", "Something went wrong", true)

	if event.Event != EventError {
		t.Errorf("expected event type %s, got %s", EventError, event.Event)
	}

	var data ErrorEventData
	if err := json.Unmarshal([]byte(event.Data), &data); err != nil {
		t.Fatalf("failed to unmarshal event data: %v", err)
	}

	if data.Code != "INTERNAL_ERROR" {
		t.Errorf("expected code 'INTERNAL_ERROR', got %s", data.Code)
	}
	if data.Message != "Something went wrong" {
		t.Errorf("expected message 'Something went wrong', got %s", data.Message)
	}
	if !data.Retryable {
		t.Error("expected retryable to be true")
	}
}

func TestNewStatusEvent(t *testing.T) {
	status := &pb.StatusUpdate{
		Status:        pb.StatusUpdate_THINKING,
		CustomMessage: "Processing your request",
		Emoji:         ":thinking:",
	}

	event := NewStatusEvent(status)

	if event.Event != EventStatus {
		t.Errorf("expected event type %s, got %s", EventStatus, event.Event)
	}

	var data StatusEventData
	if err := json.Unmarshal([]byte(event.Data), &data); err != nil {
		t.Fatalf("failed to unmarshal event data: %v", err)
	}

	if data.Message != "Processing your request" {
		t.Errorf("expected message 'Processing your request', got %s", data.Message)
	}
}

func TestNewErrorEvent(t *testing.T) {
	err := &pb.ErrorResponse{
		Code:      pb.ErrorResponse_RATE_LIMIT,
		Message:   "Too many requests",
		Details:   "Try again in 30 seconds",
		Retryable: true,
	}

	event := NewErrorEvent(err)

	if event.Event != EventError {
		t.Errorf("expected event type %s, got %s", EventError, event.Event)
	}

	var data ErrorEventData
	if jsonErr := json.Unmarshal([]byte(event.Data), &data); jsonErr != nil {
		t.Fatalf("failed to unmarshal event data: %v", jsonErr)
	}

	if data.Message != "Too many requests" {
		t.Errorf("expected message 'Too many requests', got %s", data.Message)
	}
	if !data.Retryable {
		t.Error("expected retryable to be true")
	}
}

func TestHandlers_CreateConversation(t *testing.T) {
	cm := NewConnectionManager(30 * time.Second)
	sm := &NoopSessionManager{}
	handlers := NewHandlers(cm, sm, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/conversations", nil)
	w := httptest.NewRecorder()

	handlers.HandleCreateConversation(w, req)

	if w.Code != http.StatusCreated {
		t.Errorf("expected status %d, got %d", http.StatusCreated, w.Code)
	}

	var resp CreateConversationResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if resp.ConversationID == "" {
		t.Error("expected non-empty conversation ID")
	}
	// Conversation ID should be a UUID (no prefix)
	if len(resp.ConversationID) != 36 {
		t.Errorf("expected conversation ID to be a UUID (36 chars), got %s", resp.ConversationID)
	}
}

func TestHandlers_SendMessage(t *testing.T) {
	cm := NewConnectionManager(30 * time.Second)
	sm := &NoopSessionManager{}
	threadStore := store.NewThreadHistoryStore(100, 50, time.Hour)
	handlers := NewHandlers(cm, sm, threadStore, nil)

	var receivedMsg *pb.Message
	handlers.SetMessageHandler(func(ctx context.Context, msg *pb.Message) error {
		receivedMsg = msg
		return nil
	})

	body := `{"content":"Hello, world!"}`
	req := httptest.NewRequest(http.MethodPost, "/api/conversations/conv-123/messages", bytes.NewReader([]byte(body)))
	req.SetPathValue("id", "conv-123")
	w := httptest.NewRecorder()

	handlers.HandleSendMessage(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d: %s", http.StatusOK, w.Code, w.Body.String())
	}

	var resp SendMessageResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if resp.MessageID == "" {
		t.Error("expected non-empty message ID")
	}

	if receivedMsg == nil {
		t.Fatal("expected message handler to be called")
	}
	if receivedMsg.Content != "Hello, world!" {
		t.Errorf("expected content 'Hello, world!', got %s", receivedMsg.Content)
	}
	if receivedMsg.ConversationId != "conv-123" {
		t.Errorf("expected conversation ID 'conv-123', got %s", receivedMsg.ConversationId)
	}
}

func TestHandlers_SendMessage_Unauthorized(t *testing.T) {
	cm := NewConnectionManager(30 * time.Second)
	sm := &HeaderSessionManager{UserIDHeader: "X-User-ID"} // Requires header
	handlers := NewHandlers(cm, sm, nil, nil)

	body := `{"content":"Hello"}`
	req := httptest.NewRequest(http.MethodPost, "/api/conversations/conv-123/messages", bytes.NewReader([]byte(body)))
	req.SetPathValue("id", "conv-123")
	w := httptest.NewRecorder()

	handlers.HandleSendMessage(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected status %d, got %d", http.StatusUnauthorized, w.Code)
	}
}

func TestHandlers_SendMessage_EmptyContent(t *testing.T) {
	cm := NewConnectionManager(30 * time.Second)
	sm := &NoopSessionManager{}
	handlers := NewHandlers(cm, sm, nil, nil)

	body := `{"content":""}`
	req := httptest.NewRequest(http.MethodPost, "/api/conversations/conv-123/messages", bytes.NewReader([]byte(body)))
	req.SetPathValue("id", "conv-123")
	w := httptest.NewRecorder()

	handlers.HandleSendMessage(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d: %s", http.StatusBadRequest, w.Code, w.Body.String())
	}
}

func TestHandlers_SendMessage_InvalidJSON(t *testing.T) {
	cm := NewConnectionManager(30 * time.Second)
	sm := &NoopSessionManager{}
	handlers := NewHandlers(cm, sm, nil, nil)

	body := `{invalid json}`
	req := httptest.NewRequest(http.MethodPost, "/api/conversations/conv-123/messages", bytes.NewReader([]byte(body)))
	req.SetPathValue("id", "conv-123")
	w := httptest.NewRecorder()

	handlers.HandleSendMessage(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d: %s", http.StatusBadRequest, w.Code, w.Body.String())
	}
}

func TestHandlers_SendMessage_NoHandler(t *testing.T) {
	cm := NewConnectionManager(30 * time.Second)
	sm := &NoopSessionManager{}
	handlers := NewHandlers(cm, sm, nil, nil)
	// Note: no SetMessageHandler called

	body := `{"content":"Hello"}`
	req := httptest.NewRequest(http.MethodPost, "/api/conversations/conv-123/messages", bytes.NewReader([]byte(body)))
	req.SetPathValue("id", "conv-123")
	w := httptest.NewRecorder()

	handlers.HandleSendMessage(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected status %d, got %d: %s", http.StatusServiceUnavailable, w.Code, w.Body.String())
	}
}

func TestHandlers_SendMessage_HandlerError(t *testing.T) {
	cm := NewConnectionManager(30 * time.Second)
	sm := &NoopSessionManager{}
	handlers := NewHandlers(cm, sm, nil, nil)

	handlers.SetMessageHandler(func(ctx context.Context, msg *pb.Message) error {
		return context.DeadlineExceeded // Simulate an error
	})

	body := `{"content":"Hello"}`
	req := httptest.NewRequest(http.MethodPost, "/api/conversations/conv-123/messages", bytes.NewReader([]byte(body)))
	req.SetPathValue("id", "conv-123")
	w := httptest.NewRecorder()

	handlers.HandleSendMessage(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected status %d, got %d: %s", http.StatusInternalServerError, w.Code, w.Body.String())
	}
}

func TestHandlers_SendMessage_MissingConversationID(t *testing.T) {
	cm := NewConnectionManager(30 * time.Second)
	sm := &NoopSessionManager{}
	handlers := NewHandlers(cm, sm, nil, nil)

	body := `{"content":"Hello"}`
	req := httptest.NewRequest(http.MethodPost, "/api/conversations//messages", bytes.NewReader([]byte(body)))
	// Note: not setting path value
	w := httptest.NewRecorder()

	handlers.HandleSendMessage(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d: %s", http.StatusBadRequest, w.Code, w.Body.String())
	}
}

func TestHandlers_History(t *testing.T) {
	cm := NewConnectionManager(30 * time.Second)
	sm := &NoopSessionManager{}
	threadStore := store.NewThreadHistoryStore(100, 50, time.Hour)
	handlers := NewHandlers(cm, sm, threadStore, nil)

	// Add some messages
	threadStore.AddMessage("conv-123", &pb.ThreadMessage{
		MessageId: "msg-1",
		Content:   "First message",
		User:      &pb.User{Id: "user-1", Username: "Alice"},
	})
	threadStore.AddMessage("conv-123", &pb.ThreadMessage{
		MessageId: "msg-2",
		Content:   "Second message",
		User:      &pb.User{Id: "user-2", Username: "Bob"},
	})

	req := httptest.NewRequest(http.MethodGet, "/api/conversations/conv-123/history", nil)
	req.SetPathValue("id", "conv-123")
	w := httptest.NewRecorder()

	handlers.HandleHistory(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	messages, ok := resp["messages"].([]any)
	if !ok {
		t.Fatal("expected messages array in response")
	}
	if len(messages) != 2 {
		t.Errorf("expected 2 messages, got %d", len(messages))
	}
}

func TestHandlers_Health(t *testing.T) {
	cm := NewConnectionManager(30 * time.Second)
	sm := &NoopSessionManager{}
	handlers := NewHandlers(cm, sm, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()

	handlers.HandleHealth(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if resp["status"] != "healthy" {
		t.Errorf("expected status 'healthy', got %v", resp["status"])
	}
}

func TestWebAdapter_Capabilities(t *testing.T) {
	adapter := New()
	caps := adapter.Capabilities()

	if !caps.SupportsStreaming {
		t.Error("expected SupportsStreaming to be true")
	}
	if !caps.SupportsStatusUpdates {
		t.Error("expected SupportsStatusUpdates to be true")
	}
	if !caps.SupportsSuggestedPrompts {
		t.Error("expected SupportsSuggestedPrompts to be true")
	}
	if !caps.SupportsThreads {
		t.Error("expected SupportsThreads to be true")
	}
	if caps.SupportsTypingIndicator {
		t.Error("expected SupportsTypingIndicator to be false")
	}
	if caps.MaxUpdateRateHz != 0 {
		t.Errorf("expected MaxUpdateRateHz to be 0, got %f", caps.MaxUpdateRateHz)
	}
}

func TestWebAdapter_GetPlatformName(t *testing.T) {
	adapter := New()
	if adapter.GetPlatformName() != "web" {
		t.Errorf("expected platform name 'web', got %s", adapter.GetPlatformName())
	}
}

func TestWebAdapter_HandleAgentResponse_Content(t *testing.T) {
	adapter := New()
	threadStore := store.NewThreadHistoryStore(100, 50, time.Hour)
	adapter.SetThreadStore(threadStore)

	// Initialize adapter to create connection manager
	ctx := context.Background()
	if err := adapter.Initialize(ctx, adapter.config); err != nil {
		t.Fatalf("failed to initialize adapter: %v", err)
	}

	// Create a connection to receive events
	conn := &SSEConnection{
		ID:             "test-conn",
		ConversationID: "conv-123",
		EventChan:      make(chan SSEEvent, 10),
		Done:           make(chan struct{}),
	}
	adapter.connManager.Add(conn)

	// Send a content response
	response := &pb.AgentResponse{
		ConversationId: "conv-123",
		ResponseId:     "resp-1",
		Payload: &pb.AgentResponse_Content{
			Content: &pb.ContentChunk{
				Type:    pb.ContentChunk_DELTA,
				Content: "Hello from agent",
			},
		},
	}

	if err := adapter.HandleAgentResponse(ctx, response); err != nil {
		t.Fatalf("HandleAgentResponse failed: %v", err)
	}

	// Verify event was broadcast
	select {
	case event := <-conn.EventChan:
		if event.Event != EventChunk {
			t.Errorf("expected event type %s, got %s", EventChunk, event.Event)
		}
		var data ChunkEventData
		if err := json.Unmarshal([]byte(event.Data), &data); err != nil {
			t.Fatalf("failed to unmarshal event data: %v", err)
		}
		if data.Content != "Hello from agent" {
			t.Errorf("expected content 'Hello from agent', got %s", data.Content)
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("timeout waiting for event")
	}
}

func TestWebAdapter_HandleAgentResponse_ContentEnd(t *testing.T) {
	adapter := New()
	threadStore := store.NewThreadHistoryStore(100, 50, time.Hour)
	adapter.SetThreadStore(threadStore)

	ctx := context.Background()
	if err := adapter.Initialize(ctx, adapter.config); err != nil {
		t.Fatalf("failed to initialize adapter: %v", err)
	}

	conn := &SSEConnection{
		ID:             "test-conn",
		ConversationID: "conv-123",
		EventChan:      make(chan SSEEvent, 10),
		Done:           make(chan struct{}),
	}
	adapter.connManager.Add(conn)

	// Send END chunk - should also send finish event
	response := &pb.AgentResponse{
		ConversationId: "conv-123",
		ResponseId:     "resp-1",
		Payload: &pb.AgentResponse_Content{
			Content: &pb.ContentChunk{
				Type:    pb.ContentChunk_END,
				Content: "Final content",
			},
		},
	}

	if err := adapter.HandleAgentResponse(ctx, response); err != nil {
		t.Fatalf("HandleAgentResponse failed: %v", err)
	}

	// Should receive chunk event
	event1 := <-conn.EventChan
	if event1.Event != EventChunk {
		t.Errorf("expected first event type %s, got %s", EventChunk, event1.Event)
	}

	// Should also receive finish event
	event2 := <-conn.EventChan
	if event2.Event != EventFinish {
		t.Errorf("expected second event type %s, got %s", EventFinish, event2.Event)
	}

	// Verify message was stored in thread history
	history := threadStore.GetHistory("conv-123", 10, false)
	if len(history.Messages) != 1 {
		t.Errorf("expected 1 message in history, got %d", len(history.Messages))
	}
}

func TestWebAdapter_HandleAgentResponse_Status(t *testing.T) {
	adapter := New()

	ctx := context.Background()
	if err := adapter.Initialize(ctx, adapter.config); err != nil {
		t.Fatalf("failed to initialize adapter: %v", err)
	}

	conn := &SSEConnection{
		ID:             "test-conn",
		ConversationID: "conv-123",
		EventChan:      make(chan SSEEvent, 10),
		Done:           make(chan struct{}),
	}
	adapter.connManager.Add(conn)

	response := &pb.AgentResponse{
		ConversationId: "conv-123",
		Payload: &pb.AgentResponse_Status{
			Status: &pb.StatusUpdate{
				Status:        pb.StatusUpdate_THINKING,
				CustomMessage: "Processing...",
			},
		},
	}

	if err := adapter.HandleAgentResponse(ctx, response); err != nil {
		t.Fatalf("HandleAgentResponse failed: %v", err)
	}

	select {
	case event := <-conn.EventChan:
		if event.Event != EventStatus {
			t.Errorf("expected event type %s, got %s", EventStatus, event.Event)
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("timeout waiting for event")
	}
}

func TestWebAdapter_HandleAgentResponse_Error(t *testing.T) {
	adapter := New()

	ctx := context.Background()
	if err := adapter.Initialize(ctx, adapter.config); err != nil {
		t.Fatalf("failed to initialize adapter: %v", err)
	}

	conn := &SSEConnection{
		ID:             "test-conn",
		ConversationID: "conv-123",
		EventChan:      make(chan SSEEvent, 10),
		Done:           make(chan struct{}),
	}
	adapter.connManager.Add(conn)

	response := &pb.AgentResponse{
		ConversationId: "conv-123",
		Payload: &pb.AgentResponse_Error{
			Error: &pb.ErrorResponse{
				Code:      pb.ErrorResponse_AGENT_ERROR,
				Message:   "Something went wrong",
				Retryable: false,
			},
		},
	}

	if err := adapter.HandleAgentResponse(ctx, response); err != nil {
		t.Fatalf("HandleAgentResponse failed: %v", err)
	}

	select {
	case event := <-conn.EventChan:
		if event.Event != EventError {
			t.Errorf("expected event type %s, got %s", EventError, event.Event)
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("timeout waiting for event")
	}
}

func TestWebAdapter_HandleAgentResponse_MissingConversationID(t *testing.T) {
	adapter := New()

	ctx := context.Background()
	if err := adapter.Initialize(ctx, adapter.config); err != nil {
		t.Fatalf("failed to initialize adapter: %v", err)
	}

	response := &pb.AgentResponse{
		ConversationId: "", // Missing
		Payload: &pb.AgentResponse_Content{
			Content: &pb.ContentChunk{
				Type:    pb.ContentChunk_DELTA,
				Content: "test",
			},
		},
	}

	err := adapter.HandleAgentResponse(ctx, response)
	if err == nil {
		t.Error("expected error for missing conversation ID")
	}
}

func TestWebAdapter_IsHealthy(t *testing.T) {
	adapter := New()

	// Before initialization - not healthy (no server, no connManager)
	if adapter.IsHealthy(context.Background()) {
		t.Error("expected IsHealthy to be false before initialization")
	}

	// After initialization - still not healthy (server is created in Start())
	ctx := context.Background()
	if err := adapter.Initialize(ctx, adapter.config); err != nil {
		t.Fatalf("failed to initialize adapter: %v", err)
	}

	// IsHealthy requires both server and connManager
	// Initialize only creates connManager, Start() creates server
	// So after Initialize alone, not yet healthy
	if adapter.IsHealthy(context.Background()) {
		t.Error("expected IsHealthy to be false after Initialize (server not started)")
	}

	// Verify connManager was created
	if adapter.connManager == nil {
		t.Error("expected connManager to be initialized")
	}
}

func TestWebAdapter_Options(t *testing.T) {
	// Test WithListenAddr
	adapter := New(WithListenAddr(":9090"))
	if adapter.listenAddr != ":9090" {
		t.Errorf("expected listen addr ':9090', got %s", adapter.listenAddr)
	}

	// Test WithHeartbeatInterval
	adapter2 := New(WithHeartbeatInterval(60 * time.Second))
	if adapter2.heartbeatInterval != 60*time.Second {
		t.Errorf("expected heartbeat interval 60s, got %v", adapter2.heartbeatInterval)
	}

	// Test WithSessionManager
	sm := &HeaderSessionManager{UserIDHeader: "X-User"}
	adapter3 := New(WithSessionManager(sm))
	if adapter3.sessionManager != sm {
		t.Error("expected custom session manager to be set")
	}
}

func TestHeaderSessionManager(t *testing.T) {
	sm := NewHeaderSessionManager("X-User-ID", "X-Username", "X-Email")

	// Test with valid headers
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-User-ID", "user-123")
	req.Header.Set("X-Username", "testuser")
	req.Header.Set("X-Email", "test@example.com")

	session, err := sm.ValidateRequest(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if session == nil {
		t.Fatal("expected session, got nil")
	}
	if session.UserID != "user-123" {
		t.Errorf("expected user ID 'user-123', got %s", session.UserID)
	}
	if session.Username != "testuser" {
		t.Errorf("expected username 'testuser', got %s", session.Username)
	}

	// Test without headers
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	session2, err := sm.ValidateRequest(context.Background(), req2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if session2 != nil {
		t.Error("expected nil session when no headers present")
	}
}

// --- Tests for HandleAgentConfig ---

func TestHandlers_AgentConfig_NoStore(t *testing.T) {
	cm := NewConnectionManager(30 * time.Second)
	sm := &NoopSessionManager{}
	handlers := NewHandlers(cm, sm, nil, nil) // nil config store

	req := httptest.NewRequest(http.MethodGet, "/api/agent/config", nil)
	w := httptest.NewRecorder()

	handlers.HandleAgentConfig(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected status %d, got %d", http.StatusNotFound, w.Code)
	}
}

func TestHandlers_AgentConfig_NotYetReceived(t *testing.T) {
	cm := NewConnectionManager(30 * time.Second)
	sm := &NoopSessionManager{}
	configStore := store.NewAgentConfigStore() // empty — no config set
	handlers := NewHandlers(cm, sm, nil, configStore)

	req := httptest.NewRequest(http.MethodGet, "/api/agent/config", nil)
	w := httptest.NewRecorder()

	handlers.HandleAgentConfig(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected status %d, got %d", http.StatusNotFound, w.Code)
	}
}

func TestHandlers_AgentConfig_ValidConfig(t *testing.T) {
	cm := NewConnectionManager(30 * time.Second)
	sm := &NoopSessionManager{}
	configStore := store.NewAgentConfigStore()
	configStore.Set(&pb.AgentConfig{
		SystemPrompt: "You are a helpful assistant.",
		Tools: []*pb.AgentToolConfig{
			{
				Name:        "web_search",
				Title:       "Web Search",
				Description: "Search the internet",
				Type:        "other",
			},
		},
	})
	handlers := NewHandlers(cm, sm, nil, configStore)

	req := httptest.NewRequest(http.MethodGet, "/api/agent/config", nil)
	w := httptest.NewRecorder()

	handlers.HandleAgentConfig(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d: %s", http.StatusOK, w.Code, w.Body.String())
	}

	contentType := w.Header().Get("Content-Type")
	if contentType != "application/json" {
		t.Errorf("expected Content-Type 'application/json', got %q", contentType)
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if resp["systemPrompt"] != "You are a helpful assistant." {
		t.Errorf("systemPrompt: expected 'You are a helpful assistant.', got %v", resp["systemPrompt"])
	}

	tools, ok := resp["tools"].([]any)
	if !ok {
		t.Fatal("expected tools array")
	}
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}

	tool := tools[0].(map[string]any)
	if tool["name"] != "web_search" {
		t.Errorf("tool name: expected 'web_search', got %v", tool["name"])
	}
	if tool["title"] != "Web Search" {
		t.Errorf("tool title: expected 'Web Search', got %v", tool["title"])
	}
	if tool["type"] != "other" {
		t.Errorf("tool type: expected 'other', got %v", tool["type"])
	}
}

func TestHandlers_AgentConfig_WithGraph(t *testing.T) {
	cm := NewConnectionManager(30 * time.Second)
	sm := &NoopSessionManager{}
	configStore := store.NewAgentConfigStore()
	configStore.Set(&pb.AgentConfig{
		SystemPrompt: "Graph agent",
		Tools: []*pb.AgentToolConfig{
			{
				Name:        "workflow",
				Title:       "Workflow Engine",
				Description: "Runs a workflow graph",
				Type:        "graph",
				Graph: &pb.AgentToolGraph{
					Nodes: []*pb.AgentToolGraphNode{
						{Id: "start", Name: "Start", Type: "start"},
						{Id: "llm", Name: "LLM Call", Type: "step"},
						{Id: "tools", Name: "Tool Router", Type: "step"},
					},
					Edges: []*pb.AgentToolGraphEdge{
						{Id: "e1", Source: "start", Target: "llm"},
						{Id: "e2", Source: "llm", Target: "tools"},
						{Id: "e3", Source: "tools", Target: "llm"},
					},
				},
			},
		},
	})
	handlers := NewHandlers(cm, sm, nil, configStore)

	req := httptest.NewRequest(http.MethodGet, "/api/agent/config", nil)
	w := httptest.NewRecorder()

	handlers.HandleAgentConfig(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, w.Code)
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	tools := resp["tools"].([]any)
	tool := tools[0].(map[string]any)

	graph, ok := tool["graph"].(map[string]any)
	if !ok {
		t.Fatal("expected graph object in tool")
	}

	nodes := graph["nodes"].([]any)
	if len(nodes) != 3 {
		t.Errorf("expected 3 nodes, got %d", len(nodes))
	}

	edges := graph["edges"].([]any)
	if len(edges) != 3 {
		t.Errorf("expected 3 edges, got %d", len(edges))
	}

	// Verify node structure
	node0 := nodes[0].(map[string]any)
	if node0["id"] != "start" {
		t.Errorf("first node id: expected 'start', got %v", node0["id"])
	}
	if node0["name"] != "Start" {
		t.Errorf("first node name: expected 'Start', got %v", node0["name"])
	}
	if node0["type"] != "start" {
		t.Errorf("first node type: expected 'start', got %v", node0["type"])
	}

	// Verify edge structure
	edge0 := edges[0].(map[string]any)
	if edge0["id"] != "e1" {
		t.Errorf("first edge id: expected 'e1', got %v", edge0["id"])
	}
	if edge0["source"] != "start" {
		t.Errorf("first edge source: expected 'start', got %v", edge0["source"])
	}
	if edge0["target"] != "llm" {
		t.Errorf("first edge target: expected 'llm', got %v", edge0["target"])
	}
}

func TestHandlers_AgentConfig_EmptyTools(t *testing.T) {
	cm := NewConnectionManager(30 * time.Second)
	sm := &NoopSessionManager{}
	configStore := store.NewAgentConfigStore()
	configStore.Set(&pb.AgentConfig{
		SystemPrompt: "No tools agent",
		Tools:        []*pb.AgentToolConfig{},
	})
	handlers := NewHandlers(cm, sm, nil, configStore)

	req := httptest.NewRequest(http.MethodGet, "/api/agent/config", nil)
	w := httptest.NewRecorder()

	handlers.HandleAgentConfig(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, w.Code)
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	tools := resp["tools"].([]any)
	if len(tools) != 0 {
		t.Errorf("expected 0 tools, got %d", len(tools))
	}

	if resp["systemPrompt"] != "No tools agent" {
		t.Errorf("systemPrompt: expected 'No tools agent', got %v", resp["systemPrompt"])
	}
}

func TestHandlers_AgentConfig_NilToolsInProto(t *testing.T) {
	// When protobuf deserializes, repeated fields can be nil (not empty slice)
	cm := NewConnectionManager(30 * time.Second)
	sm := &NoopSessionManager{}
	configStore := store.NewAgentConfigStore()
	configStore.Set(&pb.AgentConfig{
		SystemPrompt: "Prompt only",
		// Tools is nil (not set)
	})
	handlers := NewHandlers(cm, sm, nil, configStore)

	req := httptest.NewRequest(http.MethodGet, "/api/agent/config", nil)
	w := httptest.NewRecorder()

	handlers.HandleAgentConfig(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, w.Code)
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	// tools should be an empty JSON array, not null
	tools, ok := resp["tools"].([]any)
	if !ok {
		t.Fatalf("expected tools to be an array, got %T (%v)", resp["tools"], resp["tools"])
	}
	if len(tools) != 0 {
		t.Errorf("expected 0 tools, got %d", len(tools))
	}
}

func TestHandlers_AgentConfig_ToolWithoutGraph(t *testing.T) {
	// When type is "other" and graph is nil, JSON should omit the graph field
	cm := NewConnectionManager(30 * time.Second)
	sm := &NoopSessionManager{}
	configStore := store.NewAgentConfigStore()
	configStore.Set(&pb.AgentConfig{
		SystemPrompt: "test",
		Tools: []*pb.AgentToolConfig{
			{
				Name:        "calculator",
				Title:       "Calculator",
				Description: "Does math",
				Type:        "other",
				Graph:       nil,
			},
		},
	})
	handlers := NewHandlers(cm, sm, nil, configStore)

	req := httptest.NewRequest(http.MethodGet, "/api/agent/config", nil)
	w := httptest.NewRecorder()

	handlers.HandleAgentConfig(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, w.Code)
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	tools := resp["tools"].([]any)
	tool := tools[0].(map[string]any)

	// graph should be omitted (nil)
	if _, exists := tool["graph"]; exists {
		t.Error("expected graph field to be omitted when nil")
	}
}

func TestHandlers_AgentConfig_MultipleTools(t *testing.T) {
	cm := NewConnectionManager(30 * time.Second)
	sm := &NoopSessionManager{}
	configStore := store.NewAgentConfigStore()
	configStore.Set(&pb.AgentConfig{
		SystemPrompt: "Multi-tool agent",
		Tools: []*pb.AgentToolConfig{
			{Name: "search", Title: "Search", Description: "Search things", Type: "other"},
			{Name: "calc", Title: "Calculator", Description: "Math", Type: "other"},
			{
				Name: "workflow", Title: "Workflow", Description: "Flow", Type: "graph",
				Graph: &pb.AgentToolGraph{
					Nodes: []*pb.AgentToolGraphNode{{Id: "a", Name: "A", Type: "start"}},
					Edges: []*pb.AgentToolGraphEdge{},
				},
			},
		},
	})
	handlers := NewHandlers(cm, sm, nil, configStore)

	req := httptest.NewRequest(http.MethodGet, "/api/agent/config", nil)
	w := httptest.NewRecorder()

	handlers.HandleAgentConfig(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, w.Code)
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	tools := resp["tools"].([]any)
	if len(tools) != 3 {
		t.Errorf("expected 3 tools, got %d", len(tools))
	}

	// Third tool should have a graph
	tool2 := tools[2].(map[string]any)
	if _, ok := tool2["graph"]; !ok {
		t.Error("expected third tool to have a graph")
	}

	// First tool should NOT have a graph
	tool0 := tools[0].(map[string]any)
	if _, ok := tool0["graph"]; ok {
		t.Error("expected first tool to not have a graph")
	}
}

// --- Tests for full streaming sequence via HandleAgentResponse ---

func TestWebAdapter_HandleAgentResponse_FullStreamingSequence(t *testing.T) {
	adapter := New()
	threadStore := store.NewThreadHistoryStore(100, 50, time.Hour)
	adapter.SetThreadStore(threadStore)

	ctx := context.Background()
	if err := adapter.Initialize(ctx, adapter.config); err != nil {
		t.Fatalf("failed to initialize adapter: %v", err)
	}

	conn := &SSEConnection{
		ID:             "test-conn",
		ConversationID: "conv-stream",
		EventChan:      make(chan SSEEvent, 20),
		Done:           make(chan struct{}),
	}
	adapter.connManager.Add(conn)

	// Simulate the full agent streaming flow:
	// THINKING status → START → DELTA → DELTA → GENERATING status → DELTA → END
	responses := []*pb.AgentResponse{
		{
			ConversationId: "conv-stream",
			Payload: &pb.AgentResponse_Status{
				Status: &pb.StatusUpdate{Status: pb.StatusUpdate_THINKING},
			},
		},
		{
			ConversationId: "conv-stream",
			ResponseId:     "resp-1",
			Payload: &pb.AgentResponse_Content{
				Content: &pb.ContentChunk{Type: pb.ContentChunk_START, Content: ""},
			},
		},
		{
			ConversationId: "conv-stream",
			ResponseId:     "resp-1",
			Payload: &pb.AgentResponse_Content{
				Content: &pb.ContentChunk{Type: pb.ContentChunk_DELTA, Content: "Hello "},
			},
		},
		{
			ConversationId: "conv-stream",
			ResponseId:     "resp-1",
			Payload: &pb.AgentResponse_Content{
				Content: &pb.ContentChunk{Type: pb.ContentChunk_DELTA, Content: "world"},
			},
		},
		{
			ConversationId: "conv-stream",
			Payload: &pb.AgentResponse_Status{
				Status: &pb.StatusUpdate{Status: pb.StatusUpdate_GENERATING},
			},
		},
		{
			ConversationId: "conv-stream",
			ResponseId:     "resp-1",
			Payload: &pb.AgentResponse_Content{
				Content: &pb.ContentChunk{Type: pb.ContentChunk_DELTA, Content: "!"},
			},
		},
		{
			ConversationId: "conv-stream",
			ResponseId:     "resp-1",
			Payload: &pb.AgentResponse_Content{
				Content: &pb.ContentChunk{Type: pb.ContentChunk_END, Content: "Hello world!"},
			},
		},
	}

	for i, resp := range responses {
		if err := adapter.HandleAgentResponse(ctx, resp); err != nil {
			t.Fatalf("HandleAgentResponse failed at step %d: %v", i, err)
		}
	}

	// Collect all events (END produces chunk + finish = 8 total)
	expectedEvents := []string{
		EventStatus,  // THINKING
		EventChunk,   // START
		EventChunk,   // DELTA "Hello "
		EventChunk,   // DELTA "world"
		EventStatus,  // GENERATING
		EventChunk,   // DELTA "!"
		EventChunk,   // END
		EventFinish,  // finish (auto-sent after END)
	}

	for i, expected := range expectedEvents {
		select {
		case event := <-conn.EventChan:
			if event.Event != expected {
				t.Errorf("event %d: expected %q, got %q", i, expected, event.Event)
			}
		case <-time.After(100 * time.Millisecond):
			t.Fatalf("timeout waiting for event %d (%s)", i, expected)
		}
	}

	// Verify thread history stored the END content
	history := threadStore.GetHistory("conv-stream", 10, false)
	if len(history.Messages) != 1 {
		t.Errorf("expected 1 message in history, got %d", len(history.Messages))
	}
	if history.Messages[0].Content != "Hello world!" {
		t.Errorf("stored content: expected 'Hello world!', got %q", history.Messages[0].Content)
	}
}

func TestWebAdapter_HandleAgentResponse_ProcessingStatus(t *testing.T) {
	adapter := New()

	ctx := context.Background()
	if err := adapter.Initialize(ctx, adapter.config); err != nil {
		t.Fatalf("failed to initialize adapter: %v", err)
	}

	conn := &SSEConnection{
		ID:             "test-conn",
		ConversationID: "conv-tools",
		EventChan:      make(chan SSEEvent, 10),
		Done:           make(chan struct{}),
	}
	adapter.connManager.Add(conn)

	// Send PROCESSING status with custom message (tool call)
	response := &pb.AgentResponse{
		ConversationId: "conv-tools",
		Payload: &pb.AgentResponse_Status{
			Status: &pb.StatusUpdate{
				Status:        pb.StatusUpdate_PROCESSING,
				CustomMessage: "Running search_docs",
				Emoji:         "🔧",
			},
		},
	}

	if err := adapter.HandleAgentResponse(ctx, response); err != nil {
		t.Fatalf("HandleAgentResponse failed: %v", err)
	}

	select {
	case event := <-conn.EventChan:
		if event.Event != EventStatus {
			t.Errorf("expected event type %s, got %s", EventStatus, event.Event)
		}
		var data StatusEventData
		if err := json.Unmarshal([]byte(event.Data), &data); err != nil {
			t.Fatalf("failed to unmarshal: %v", err)
		}
		if data.Status != "PROCESSING" {
			t.Errorf("expected status PROCESSING, got %s", data.Status)
		}
		if data.Message != "Running search_docs" {
			t.Errorf("expected message 'Running search_docs', got %q", data.Message)
		}
		if data.Emoji != "🔧" {
			t.Errorf("expected emoji '🔧', got %q", data.Emoji)
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("timeout waiting for event")
	}
}

func TestWebAdapter_HandleAgentResponse_AnalyzingStatus(t *testing.T) {
	adapter := New()

	ctx := context.Background()
	if err := adapter.Initialize(ctx, adapter.config); err != nil {
		t.Fatalf("failed to initialize adapter: %v", err)
	}

	conn := &SSEConnection{
		ID:             "test-conn",
		ConversationID: "conv-tools",
		EventChan:      make(chan SSEEvent, 10),
		Done:           make(chan struct{}),
	}
	adapter.connManager.Add(conn)

	response := &pb.AgentResponse{
		ConversationId: "conv-tools",
		Payload: &pb.AgentResponse_Status{
			Status: &pb.StatusUpdate{
				Status:        pb.StatusUpdate_ANALYZING,
				CustomMessage: "Finished search_docs",
			},
		},
	}

	if err := adapter.HandleAgentResponse(ctx, response); err != nil {
		t.Fatalf("HandleAgentResponse failed: %v", err)
	}

	select {
	case event := <-conn.EventChan:
		var data StatusEventData
		if err := json.Unmarshal([]byte(event.Data), &data); err != nil {
			t.Fatalf("failed to unmarshal: %v", err)
		}
		if data.Status != "ANALYZING" {
			t.Errorf("expected status ANALYZING, got %s", data.Status)
		}
		if data.Message != "Finished search_docs" {
			t.Errorf("expected message 'Finished search_docs', got %q", data.Message)
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("timeout waiting for event")
	}
}

func TestWebAdapter_HandleAgentResponse_StartChunkData(t *testing.T) {
	adapter := New()

	ctx := context.Background()
	if err := adapter.Initialize(ctx, adapter.config); err != nil {
		t.Fatalf("failed to initialize adapter: %v", err)
	}

	conn := &SSEConnection{
		ID:             "test-conn",
		ConversationID: "conv-1",
		EventChan:      make(chan SSEEvent, 10),
		Done:           make(chan struct{}),
	}
	adapter.connManager.Add(conn)

	response := &pb.AgentResponse{
		ConversationId: "conv-1",
		ResponseId:     "resp-1",
		Payload: &pb.AgentResponse_Content{
			Content: &pb.ContentChunk{Type: pb.ContentChunk_START, Content: ""},
		},
	}

	if err := adapter.HandleAgentResponse(ctx, response); err != nil {
		t.Fatalf("HandleAgentResponse failed: %v", err)
	}

	select {
	case event := <-conn.EventChan:
		var data ChunkEventData
		if err := json.Unmarshal([]byte(event.Data), &data); err != nil {
			t.Fatalf("failed to unmarshal: %v", err)
		}
		if data.ChunkType != "start" {
			t.Errorf("expected chunk_type 'start', got %q", data.ChunkType)
		}
		if data.ResponseID != "resp-1" {
			t.Errorf("expected response_id 'resp-1', got %q", data.ResponseID)
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("timeout waiting for event")
	}
}

func TestWebAdapter_HandleAgentResponse_NoEventsToWrongConversation(t *testing.T) {
	adapter := New()

	ctx := context.Background()
	if err := adapter.Initialize(ctx, adapter.config); err != nil {
		t.Fatalf("failed to initialize adapter: %v", err)
	}

	conn := &SSEConnection{
		ID:             "test-conn",
		ConversationID: "conv-A",
		EventChan:      make(chan SSEEvent, 10),
		Done:           make(chan struct{}),
	}
	adapter.connManager.Add(conn)

	// Send to conv-B — conn-A should not receive it
	response := &pb.AgentResponse{
		ConversationId: "conv-B",
		Payload: &pb.AgentResponse_Content{
			Content: &pb.ContentChunk{Type: pb.ContentChunk_DELTA, Content: "wrong conversation"},
		},
	}

	if err := adapter.HandleAgentResponse(ctx, response); err != nil {
		t.Fatalf("HandleAgentResponse failed: %v", err)
	}

	select {
	case event := <-conn.EventChan:
		t.Errorf("should not have received event, got: %s", event.Event)
	case <-time.After(50 * time.Millisecond):
		// Expected — no event received
	}
}

func TestWebAdapter_HandleAgentResponse_ThreadMetadata(t *testing.T) {
	adapter := New()

	ctx := context.Background()
	if err := adapter.Initialize(ctx, adapter.config); err != nil {
		t.Fatalf("failed to initialize adapter: %v", err)
	}

	conn := &SSEConnection{
		ID:             "test-conn",
		ConversationID: "conv-meta",
		EventChan:      make(chan SSEEvent, 10),
		Done:           make(chan struct{}),
	}
	adapter.connManager.Add(conn)

	// ThreadMetadata is just logged, no SSE event emitted
	response := &pb.AgentResponse{
		ConversationId: "conv-meta",
		Payload: &pb.AgentResponse_ThreadMetadata{
			ThreadMetadata: &pb.ThreadMetadata{
				ThreadId: "thread-123",
				Title:    "Metadata Title",
			},
		},
	}

	if err := adapter.HandleAgentResponse(ctx, response); err != nil {
		t.Fatalf("HandleAgentResponse failed: %v", err)
	}

	// No SSE event should be emitted for ThreadMetadata
	select {
	case event := <-conn.EventChan:
		t.Errorf("should not have received event for ThreadMetadata, got: %s", event.Event)
	case <-time.After(50 * time.Millisecond):
		// Expected — no event
	}
}

func TestWebAdapter_HandleAgentResponse_Prompts(t *testing.T) {
	adapter := New()

	ctx := context.Background()
	if err := adapter.Initialize(ctx, adapter.config); err != nil {
		t.Fatalf("failed to initialize adapter: %v", err)
	}

	conn := &SSEConnection{
		ID:             "test-conn",
		ConversationID: "conv-prompts",
		EventChan:      make(chan SSEEvent, 10),
		Done:           make(chan struct{}),
	}
	adapter.connManager.Add(conn)

	response := &pb.AgentResponse{
		ConversationId: "conv-prompts",
		Payload: &pb.AgentResponse_Prompts{
			Prompts: &pb.SuggestedPrompts{
				Prompts: []*pb.SuggestedPrompts_Prompt{
					{Title: "Help", Message: "How can I help?"},
				},
			},
		},
	}

	if err := adapter.HandleAgentResponse(ctx, response); err != nil {
		t.Fatalf("HandleAgentResponse failed: %v", err)
	}

	select {
	case event := <-conn.EventChan:
		if event.Event != "prompts" {
			t.Errorf("expected 'prompts' event, got %q", event.Event)
		}
	case <-time.After(500 * time.Millisecond):
		t.Error("timed out waiting for prompts event")
	}
}

func TestWebAdapter_HandleAgentResponse_NilPayload(t *testing.T) {
	adapter := New()

	ctx := context.Background()
	if err := adapter.Initialize(ctx, adapter.config); err != nil {
		t.Fatalf("failed to initialize adapter: %v", err)
	}

	// Response with nil payload should not error (unhandled type logs and returns nil)
	response := &pb.AgentResponse{
		ConversationId: "conv-nil",
	}

	err := adapter.HandleAgentResponse(ctx, response)
	if err != nil {
		t.Errorf("expected nil error for unhandled payload type, got: %v", err)
	}
}

func TestWebAdapter_HandleAgentResponse_ContentReplace(t *testing.T) {
	adapter := New()

	ctx := context.Background()
	if err := adapter.Initialize(ctx, adapter.config); err != nil {
		t.Fatalf("failed to initialize adapter: %v", err)
	}

	conn := &SSEConnection{
		ID:             "test-conn",
		ConversationID: "conv-replace",
		EventChan:      make(chan SSEEvent, 10),
		Done:           make(chan struct{}),
	}
	adapter.connManager.Add(conn)

	response := &pb.AgentResponse{
		ConversationId: "conv-replace",
		Payload: &pb.AgentResponse_Content{
			Content: &pb.ContentChunk{
				Type:    pb.ContentChunk_REPLACE,
				Content: "Replaced content",
			},
		},
	}

	if err := adapter.HandleAgentResponse(ctx, response); err != nil {
		t.Fatalf("HandleAgentResponse failed: %v", err)
	}

	select {
	case event := <-conn.EventChan:
		if event.Event != "chunk" {
			t.Errorf("expected 'chunk' event, got %q", event.Event)
		}
	case <-time.After(500 * time.Millisecond):
		t.Error("timed out waiting for chunk event")
	}
}

func TestBearerTokenSessionManager(t *testing.T) {
	sm := NewBearerTokenSessionManager(func(ctx context.Context, token string) (*Session, error) {
		if token == "valid-token" {
			return &Session{
				UserID:   "user-from-token",
				Username: "tokenuser",
			}, nil
		}
		return nil, nil
	})

	// Test with valid token
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer valid-token")

	session, err := sm.ValidateRequest(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if session == nil {
		t.Fatal("expected session, got nil")
	}
	if session.UserID != "user-from-token" {
		t.Errorf("expected user ID 'user-from-token', got %s", session.UserID)
	}

	// Test with invalid token
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2.Header.Set("Authorization", "Bearer invalid-token")

	session2, err := sm.ValidateRequest(context.Background(), req2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if session2 != nil {
		t.Error("expected nil session for invalid token")
	}

	// Test without authorization header
	req3 := httptest.NewRequest(http.MethodGet, "/", nil)
	session3, err := sm.ValidateRequest(context.Background(), req3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if session3 != nil {
		t.Error("expected nil session when no auth header")
	}
}
