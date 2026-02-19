package slack

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	pb "github.com/astromode-ai/astro-messaging/pkg/gen/astro/messaging/v1"
	"github.com/astromode-ai/astro-messaging/internal/adapter"
	slackapi "github.com/slack-go/slack"
)

// slackCall records a call made to the mock Slack API
type slackCall struct {
	Method string
	Body   map[string]interface{}
}

// newTestSlackAdapter creates a SlackAdapter with a mock Slack API server.
// Returns the adapter, a slice that collects API calls, and a cleanup function.
func newTestSlackAdapter(t *testing.T) (*SlackAdapter, *[]slackCall, func()) {
	t.Helper()

	var (
		calls   []slackCall
		callsMu sync.Mutex
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Parse the body to record the call
		var body map[string]interface{}
		ct := r.Header.Get("Content-Type")
		if strings.Contains(ct, "application/json") {
			json.NewDecoder(r.Body).Decode(&body)
		} else {
			r.ParseForm()
			body = make(map[string]interface{})
			for k, v := range r.Form {
				if len(v) == 1 {
					body[k] = v[0]
				} else {
					body[k] = v
				}
			}
		}

		callsMu.Lock()
		calls = append(calls, slackCall{
			Method: r.URL.Path,
			Body:   body,
		})
		callsMu.Unlock()

		// Return a successful Slack API response
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"ok":      true,
			"ts":      "1234567890.000001",
			"channel": "C123",
		})
	}))

	// Create Slack client pointing at mock server
	client := slackapi.New("xoxb-test-token", slackapi.OptionAPIURL(server.URL+"/"))

	// Create AI client pointing at mock server
	aiClient := &SlackAIClient{
		botToken:   "xoxb-test-token",
		httpClient: server.Client(),
		baseURL:    server.URL,
	}

	a := &SlackAdapter{
		client:         client,
		aiClient:       aiClient,
		rateLimiter:    NewRateLimiter(100, 100), // high limits for tests
		config:         adapter.Config{},
		contentBuffers: make(map[string]string),
	}

	getCalls := func() []slackCall {
		callsMu.Lock()
		defer callsMu.Unlock()
		result := make([]slackCall, len(calls))
		copy(result, calls)
		return result
	}
	_ = getCalls // available for direct use in tests

	return a, &calls, server.Close
}

// --- Tests for HandleAgentResponse ---

func TestSlackAdapter_HandleAgentResponse_ContentEnd_PostsMessage(t *testing.T) {
	a, calls, cleanup := newTestSlackAdapter(t)
	defer cleanup()

	convID := "C123-1234567890.000001"

	// Send START → DELTA → END sequence; Slack adapter buffers DELTAs and posts on END
	for _, resp := range []*pb.AgentResponse{
		{ConversationId: convID, Payload: &pb.AgentResponse_Content{Content: &pb.ContentChunk{Type: pb.ContentChunk_START}}},
		{ConversationId: convID, Payload: &pb.AgentResponse_Content{Content: &pb.ContentChunk{Type: pb.ContentChunk_DELTA, Content: "Here is my answer"}}},
		{ConversationId: convID, Payload: &pb.AgentResponse_Content{Content: &pb.ContentChunk{Type: pb.ContentChunk_END}}},
	} {
		if err := a.HandleAgentResponse(t.Context(), resp); err != nil {
			t.Fatalf("HandleAgentResponse failed: %v", err)
		}
	}

	// Should have called chat.postMessage once on END
	found := false
	for _, call := range *calls {
		if call.Method == "/chat.postMessage" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected chat.postMessage call for END chunk")
	}
}

func TestSlackAdapter_HandleAgentResponse_DeltaIgnored(t *testing.T) {
	a, calls, cleanup := newTestSlackAdapter(t)
	defer cleanup()

	response := &pb.AgentResponse{
		ConversationId: "C123-1234567890.000001",
		Payload: &pb.AgentResponse_Content{
			Content: &pb.ContentChunk{
				Type:    pb.ContentChunk_DELTA,
				Content: "partial token",
			},
		},
	}

	err := a.HandleAgentResponse(t.Context(), response)
	if err != nil {
		t.Fatalf("HandleAgentResponse failed: %v", err)
	}

	// DELTA should be ignored — no API calls
	if len(*calls) != 0 {
		t.Errorf("expected no API calls for DELTA chunk, got %d", len(*calls))
	}
}

func TestSlackAdapter_HandleAgentResponse_StartIgnored(t *testing.T) {
	a, calls, cleanup := newTestSlackAdapter(t)
	defer cleanup()

	response := &pb.AgentResponse{
		ConversationId: "C123-1234567890.000001",
		Payload: &pb.AgentResponse_Content{
			Content: &pb.ContentChunk{
				Type:    pb.ContentChunk_START,
				Content: "",
			},
		},
	}

	err := a.HandleAgentResponse(t.Context(), response)
	if err != nil {
		t.Fatalf("HandleAgentResponse failed: %v", err)
	}

	if len(*calls) != 0 {
		t.Errorf("expected no API calls for START chunk, got %d", len(*calls))
	}
}

func TestSlackAdapter_HandleAgentResponse_StatusThinking(t *testing.T) {
	a, calls, cleanup := newTestSlackAdapter(t)
	defer cleanup()

	response := &pb.AgentResponse{
		ConversationId: "C123-1234567890.000001",
		Payload: &pb.AgentResponse_Status{
			Status: &pb.StatusUpdate{
				Status: pb.StatusUpdate_THINKING,
			},
		},
	}

	err := a.HandleAgentResponse(t.Context(), response)
	if err != nil {
		t.Fatalf("HandleAgentResponse failed: %v", err)
	}

	// Should call assistant.threads.setStatus
	found := false
	for _, call := range *calls {
		if call.Method == "/assistant.threads.setStatus" {
			found = true
			if call.Body["status"] != "Thinking..." {
				t.Errorf("expected status 'Thinking...', got %v", call.Body["status"])
			}
			break
		}
	}
	if !found {
		t.Error("expected assistant.threads.setStatus call for THINKING status")
	}
}

func TestSlackAdapter_HandleAgentResponse_StatusProcessingWithCustomMessage(t *testing.T) {
	a, calls, cleanup := newTestSlackAdapter(t)
	defer cleanup()

	response := &pb.AgentResponse{
		ConversationId: "C123-1234567890.000001",
		Payload: &pb.AgentResponse_Status{
			Status: &pb.StatusUpdate{
				Status:        pb.StatusUpdate_PROCESSING,
				CustomMessage: "Running search_docs",
				Emoji:         "🔧",
			},
		},
	}

	err := a.HandleAgentResponse(t.Context(), response)
	if err != nil {
		t.Fatalf("HandleAgentResponse failed: %v", err)
	}

	found := false
	for _, call := range *calls {
		if call.Method == "/assistant.threads.setStatus" {
			found = true
			// customMessage overrides default
			if call.Body["status"] != "Running search_docs" {
				t.Errorf("expected status 'Running search_docs', got %v", call.Body["status"])
			}
			break
		}
	}
	if !found {
		t.Error("expected assistant.threads.setStatus call")
	}
}

func TestSlackAdapter_HandleAgentResponse_StatusAnalyzing(t *testing.T) {
	a, calls, cleanup := newTestSlackAdapter(t)
	defer cleanup()

	response := &pb.AgentResponse{
		ConversationId: "C123-1234567890.000001",
		Payload: &pb.AgentResponse_Status{
			Status: &pb.StatusUpdate{
				Status:        pb.StatusUpdate_ANALYZING,
				CustomMessage: "Finished search_docs",
			},
		},
	}

	err := a.HandleAgentResponse(t.Context(), response)
	if err != nil {
		t.Fatalf("HandleAgentResponse failed: %v", err)
	}

	found := false
	for _, call := range *calls {
		if call.Method == "/assistant.threads.setStatus" {
			found = true
			if call.Body["status"] != "Finished search_docs" {
				t.Errorf("expected status 'Finished search_docs', got %v", call.Body["status"])
			}
			break
		}
	}
	if !found {
		t.Error("expected assistant.threads.setStatus call")
	}
}

func TestSlackAdapter_HandleAgentResponse_ErrorPostsWarning(t *testing.T) {
	a, calls, cleanup := newTestSlackAdapter(t)
	defer cleanup()

	response := &pb.AgentResponse{
		ConversationId: "C123-1234567890.000001",
		Payload: &pb.AgentResponse_Error{
			Error: &pb.ErrorResponse{
				Code:    pb.ErrorResponse_AGENT_ERROR,
				Message: "LLM rate limited",
			},
		},
	}

	err := a.HandleAgentResponse(t.Context(), response)
	if err != nil {
		t.Fatalf("HandleAgentResponse failed: %v", err)
	}

	// Should call chat.postMessage with warning
	found := false
	for _, call := range *calls {
		if call.Method == "/chat.postMessage" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected chat.postMessage call for error response")
	}
}

func TestSlackAdapter_HandleAgentResponse_NilResponse(t *testing.T) {
	a, _, cleanup := newTestSlackAdapter(t)
	defer cleanup()

	err := a.HandleAgentResponse(t.Context(), nil)
	if err == nil {
		t.Error("expected error for nil response")
	}
}

func TestSlackAdapter_HandleAgentResponse_InvalidConversationID(t *testing.T) {
	a, _, cleanup := newTestSlackAdapter(t)
	defer cleanup()

	response := &pb.AgentResponse{
		ConversationId: "",
		Payload: &pb.AgentResponse_Content{
			Content: &pb.ContentChunk{
				Type:    pb.ContentChunk_END,
				Content: "test",
			},
		},
	}

	err := a.HandleAgentResponse(t.Context(), response)
	if err == nil {
		t.Error("expected error for empty conversation ID")
	}
}

func TestSlackAdapter_HandleAgentResponse_FullStreamingSequence(t *testing.T) {
	a, calls, cleanup := newTestSlackAdapter(t)
	defer cleanup()

	convID := "C123-1234567890.000001"

	// Simulate: THINKING → START → DELTA → DELTA → PROCESSING → ANALYZING → DELTA → END
	responses := []*pb.AgentResponse{
		{
			ConversationId: convID,
			Payload: &pb.AgentResponse_Status{
				Status: &pb.StatusUpdate{Status: pb.StatusUpdate_THINKING},
			},
		},
		{
			ConversationId: convID,
			Payload: &pb.AgentResponse_Content{
				Content: &pb.ContentChunk{Type: pb.ContentChunk_START, Content: ""},
			},
		},
		{
			ConversationId: convID,
			Payload: &pb.AgentResponse_Content{
				Content: &pb.ContentChunk{Type: pb.ContentChunk_DELTA, Content: "Hello "},
			},
		},
		{
			ConversationId: convID,
			Payload: &pb.AgentResponse_Content{
				Content: &pb.ContentChunk{Type: pb.ContentChunk_DELTA, Content: "world"},
			},
		},
		{
			ConversationId: convID,
			Payload: &pb.AgentResponse_Status{
				Status: &pb.StatusUpdate{
					Status:        pb.StatusUpdate_PROCESSING,
					CustomMessage: "Running calculator",
				},
			},
		},
		{
			ConversationId: convID,
			Payload: &pb.AgentResponse_Status{
				Status: &pb.StatusUpdate{
					Status:        pb.StatusUpdate_ANALYZING,
					CustomMessage: "Finished calculator",
				},
			},
		},
		{
			ConversationId: convID,
			Payload: &pb.AgentResponse_Content{
				Content: &pb.ContentChunk{Type: pb.ContentChunk_DELTA, Content: "!"},
			},
		},
		{
			ConversationId: convID,
			Payload: &pb.AgentResponse_Content{
				Content: &pb.ContentChunk{Type: pb.ContentChunk_END, Content: ""},
			},
		},
	}

	for i, resp := range responses {
		if err := a.HandleAgentResponse(t.Context(), resp); err != nil {
			t.Fatalf("HandleAgentResponse failed at step %d: %v", i, err)
		}
	}

	// Count API calls by type
	statusCalls := 0
	messageCalls := 0
	for _, call := range *calls {
		switch call.Method {
		case "/assistant.threads.setStatus":
			statusCalls++
		case "/chat.postMessage":
			messageCalls++
		}
	}

	// 3 status calls: THINKING, PROCESSING, ANALYZING
	if statusCalls != 3 {
		t.Errorf("expected 3 status API calls, got %d", statusCalls)
	}

	// 1 message call: only END sends a message (START/DELTA ignored)
	if messageCalls != 1 {
		t.Errorf("expected 1 message API call (only END), got %d", messageCalls)
	}
}

func TestSlackAdapter_HandleAgentResponse_ReplaceChunkPostsMessage(t *testing.T) {
	a, calls, cleanup := newTestSlackAdapter(t)
	defer cleanup()

	response := &pb.AgentResponse{
		ConversationId: "C123-1234567890.000001",
		Payload: &pb.AgentResponse_Content{
			Content: &pb.ContentChunk{
				Type:    pb.ContentChunk_REPLACE,
				Content: "Replaced content",
			},
		},
	}

	err := a.HandleAgentResponse(t.Context(), response)
	if err != nil {
		t.Fatalf("HandleAgentResponse failed: %v", err)
	}

	found := false
	for _, call := range *calls {
		if call.Method == "/chat.postMessage" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected chat.postMessage call for REPLACE chunk")
	}
}

// --- Tests for status message mapping ---

func TestMapStatusToMessage(t *testing.T) {
	a := &SlackAdapter{}

	tests := []struct {
		status   *pb.StatusUpdate
		expected string
	}{
		{&pb.StatusUpdate{Status: pb.StatusUpdate_THINKING}, "Thinking..."},
		{&pb.StatusUpdate{Status: pb.StatusUpdate_SEARCHING}, "Searching..."},
		{&pb.StatusUpdate{Status: pb.StatusUpdate_GENERATING}, "Generating response..."},
		{&pb.StatusUpdate{Status: pb.StatusUpdate_PROCESSING}, "Processing..."},
		{&pb.StatusUpdate{Status: pb.StatusUpdate_ANALYZING}, "Analyzing..."},
		{&pb.StatusUpdate{Status: pb.StatusUpdate_CUSTOM, CustomMessage: "Doing stuff"}, "Doing stuff"},
		{&pb.StatusUpdate{Status: pb.StatusUpdate_PROCESSING, CustomMessage: "Running search"}, "Running search"},
	}

	for _, tt := range tests {
		result := a.mapStatusToMessage(tt.status)
		if result != tt.expected {
			t.Errorf("mapStatusToMessage(%v): expected %q, got %q", tt.status.Status, tt.expected, result)
		}
	}
}

func TestGetDefaultEmojiForStatus(t *testing.T) {
	a := &SlackAdapter{}

	tests := []struct {
		status   pb.StatusUpdate_Status
		expected string
	}{
		{pb.StatusUpdate_THINKING, ":thought_balloon:"},
		{pb.StatusUpdate_SEARCHING, ":mag:"},
		{pb.StatusUpdate_GENERATING, ":pencil2:"},
		{pb.StatusUpdate_PROCESSING, ":gear:"},
		{pb.StatusUpdate_ANALYZING, ":bar_chart:"},
	}

	for _, tt := range tests {
		result := a.getDefaultEmojiForStatus(tt.status)
		if result != tt.expected {
			t.Errorf("getDefaultEmojiForStatus(%v): expected %q, got %q", tt.status, tt.expected, result)
		}
	}
}

func TestGetDefaultEmojiForStatus_UnknownStatus(t *testing.T) {
	a := &SlackAdapter{}
	result := a.getDefaultEmojiForStatus(pb.StatusUpdate_Status(999))
	if result != ":robot_face:" {
		t.Errorf("expected ':robot_face:' for unknown status, got %q", result)
	}
}

func TestMapStatusToMessage_UnknownStatus(t *testing.T) {
	a := &SlackAdapter{}
	result := a.mapStatusToMessage(&pb.StatusUpdate{Status: pb.StatusUpdate_Status(999)})
	if result != "Working..." {
		t.Errorf("expected 'Working...' for unknown status, got %q", result)
	}
}

// --- Tests for nil payload handling ---

func TestSlackAdapter_HandleAgentResponse_NilStatus(t *testing.T) {
	a, _, cleanup := newTestSlackAdapter(t)
	defer cleanup()

	response := &pb.AgentResponse{
		ConversationId: "C123-1234567890.000001",
		Payload: &pb.AgentResponse_Status{
			Status: nil,
		},
	}

	err := a.HandleAgentResponse(t.Context(), response)
	if err == nil {
		t.Error("expected error for nil status")
	}
	if !strings.Contains(err.Error(), "nil status") {
		t.Errorf("expected 'nil status' error, got: %v", err)
	}
}

func TestSlackAdapter_HandleAgentResponse_NilContent(t *testing.T) {
	a, _, cleanup := newTestSlackAdapter(t)
	defer cleanup()

	response := &pb.AgentResponse{
		ConversationId: "C123-1234567890.000001",
		Payload: &pb.AgentResponse_Content{
			Content: nil,
		},
	}

	err := a.HandleAgentResponse(t.Context(), response)
	if err == nil {
		t.Error("expected error for nil content")
	}
	if !strings.Contains(err.Error(), "nil content") {
		t.Errorf("expected 'nil content' error, got: %v", err)
	}
}

func TestSlackAdapter_HandleAgentResponse_NilError(t *testing.T) {
	a, _, cleanup := newTestSlackAdapter(t)
	defer cleanup()

	response := &pb.AgentResponse{
		ConversationId: "C123-1234567890.000001",
		Payload: &pb.AgentResponse_Error{
			Error: nil,
		},
	}

	err := a.HandleAgentResponse(t.Context(), response)
	if err == nil {
		t.Error("expected error for nil error payload")
	}
	if !strings.Contains(err.Error(), "nil error") {
		t.Errorf("expected 'nil error' error, got: %v", err)
	}
}

func TestSlackAdapter_HandleAgentResponse_NilPrompts(t *testing.T) {
	a, _, cleanup := newTestSlackAdapter(t)
	defer cleanup()

	response := &pb.AgentResponse{
		ConversationId: "C123-1234567890.000001",
		Payload: &pb.AgentResponse_Prompts{
			Prompts: nil,
		},
	}

	err := a.HandleAgentResponse(t.Context(), response)
	if err == nil {
		t.Error("expected error for nil prompts")
	}
	if !strings.Contains(err.Error(), "nil or empty prompts") {
		t.Errorf("expected 'nil or empty prompts' error, got: %v", err)
	}
}

func TestSlackAdapter_HandleAgentResponse_NilThreadMetadata(t *testing.T) {
	a, _, cleanup := newTestSlackAdapter(t)
	defer cleanup()

	response := &pb.AgentResponse{
		ConversationId: "C123-1234567890.000001",
		Payload: &pb.AgentResponse_ThreadMetadata{
			ThreadMetadata: nil,
		},
	}

	err := a.HandleAgentResponse(t.Context(), response)
	if err == nil {
		t.Error("expected error for nil thread metadata")
	}
	if !strings.Contains(err.Error(), "nil thread metadata") {
		t.Errorf("expected 'nil thread metadata' error, got: %v", err)
	}
}

// --- Tests for parseConversationID edge cases ---

func TestParseConversationID_EmptyString(t *testing.T) {
	a := &SlackAdapter{}
	_, _, err := a.parseConversationID("")
	if err == nil {
		t.Error("expected error for empty string")
	}
}

func TestParseConversationID_ValidFormat(t *testing.T) {
	a := &SlackAdapter{}
	channelID, threadTS, err := a.parseConversationID("C123-1234567890.000001")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if channelID != "C123" {
		t.Errorf("expected channel 'C123', got %q", channelID)
	}
	if threadTS != "1234567890.000001" {
		t.Errorf("expected thread '1234567890.000001', got %q", threadTS)
	}
}

func TestParseConversationID_ChannelOnly(t *testing.T) {
	a := &SlackAdapter{}
	channelID, threadTS, err := a.parseConversationID("C0A8Y3S92BG")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if channelID != "C0A8Y3S92BG" {
		t.Errorf("expected channel 'C0A8Y3S92BG', got %q", channelID)
	}
	if threadTS != "" {
		t.Errorf("expected empty thread, got %q", threadTS)
	}
}

// --- Tests for Slack API failure propagation ---

func TestSlackAdapter_HandleAgentResponse_StatusAPIFailure(t *testing.T) {
	// Create a server that returns API errors
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"ok":    false,
			"error": "not_authed",
		})
	}))
	defer server.Close()

	client := slackapi.New("xoxb-test-token", slackapi.OptionAPIURL(server.URL+"/"))
	aiClient := &SlackAIClient{
		botToken:   "xoxb-test-token",
		httpClient: server.Client(),
		baseURL:    server.URL,
	}
	a := &SlackAdapter{
		client:      client,
		aiClient:    aiClient,
		rateLimiter: NewRateLimiter(100, 100),
		config:      adapter.Config{},
	}

	response := &pb.AgentResponse{
		ConversationId: "C123-1234567890.000001",
		Payload: &pb.AgentResponse_Status{
			Status: &pb.StatusUpdate{Status: pb.StatusUpdate_THINKING},
		},
	}

	err := a.HandleAgentResponse(t.Context(), response)
	if err == nil {
		t.Fatal("expected error when Slack API returns error")
	}
	if !strings.Contains(err.Error(), "not_authed") {
		t.Errorf("expected 'not_authed' in error, got: %v", err)
	}
}

func TestSlackAdapter_HandleAgentResponse_PromptsWithData(t *testing.T) {
	a, calls, cleanup := newTestSlackAdapter(t)
	defer cleanup()

	response := &pb.AgentResponse{
		ConversationId: "C123-1234567890.000001",
		Payload: &pb.AgentResponse_Prompts{
			Prompts: &pb.SuggestedPrompts{
				Prompts: []*pb.SuggestedPrompts_Prompt{
					{Title: "Help", Message: "How can you help me?"},
					{Title: "Status", Message: "What's the current status?"},
				},
			},
		},
	}

	err := a.HandleAgentResponse(t.Context(), response)
	if err != nil {
		t.Fatalf("HandleAgentResponse failed: %v", err)
	}

	found := false
	for _, call := range *calls {
		if call.Method == "/assistant.threads.setSuggestedPrompts" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected assistant.threads.setSuggestedPrompts call")
	}
}

func TestSlackAdapter_HandleAgentResponse_ThreadMetadata(t *testing.T) {
	a, _, cleanup := newTestSlackAdapter(t)
	defer cleanup()

	response := &pb.AgentResponse{
		ConversationId: "C123-1234567890.000001",
		Payload: &pb.AgentResponse_ThreadMetadata{
			ThreadMetadata: &pb.ThreadMetadata{
				ThreadId: "thread-123",
				Title:    "Test Thread",
			},
		},
	}

	err := a.HandleAgentResponse(t.Context(), response)
	if err != nil {
		t.Fatalf("HandleAgentResponse failed: %v", err)
	}
	// ThreadMetadata is informational only — just verifying no error
}

func TestSlackAdapter_HandleAgentResponse_ErrorWithCodeAppended(t *testing.T) {
	a, calls, cleanup := newTestSlackAdapter(t)
	defer cleanup()

	response := &pb.AgentResponse{
		ConversationId: "C123-1234567890.000001",
		Payload: &pb.AgentResponse_Error{
			Error: &pb.ErrorResponse{
				Code:    pb.ErrorResponse_AGENT_ERROR,
				Message: "Something went wrong",
			},
		},
	}

	err := a.HandleAgentResponse(t.Context(), response)
	if err != nil {
		t.Fatalf("HandleAgentResponse failed: %v", err)
	}

	// Verify the error message includes the error code
	for _, call := range *calls {
		if call.Method == "/chat.postMessage" {
			text, ok := call.Body["text"].(string)
			if !ok {
				t.Error("expected text field in body")
				continue
			}
			if !strings.Contains(text, "AGENT_ERROR") {
				t.Errorf("expected error code in message, got: %s", text)
			}
			if !strings.Contains(text, "Something went wrong") {
				t.Errorf("expected error message in text, got: %s", text)
			}
		}
	}
}

// --- Tests for feedback buttons ---

func TestSlackAdapter_HandleAgentResponse_ContentEnd_IncludesFeedbackButtons(t *testing.T) {
	a, calls, cleanup := newTestSlackAdapter(t)
	defer cleanup()

	convID := "C123-1234567890.000001"

	// Send START → DELTA → END
	for _, resp := range []*pb.AgentResponse{
		{ConversationId: convID, Payload: &pb.AgentResponse_Content{Content: &pb.ContentChunk{Type: pb.ContentChunk_START}}},
		{ConversationId: convID, Payload: &pb.AgentResponse_Content{Content: &pb.ContentChunk{Type: pb.ContentChunk_DELTA, Content: "Hello!"}}},
		{ConversationId: convID, Payload: &pb.AgentResponse_Content{Content: &pb.ContentChunk{Type: pb.ContentChunk_END}}},
	} {
		if err := a.HandleAgentResponse(t.Context(), resp); err != nil {
			t.Fatalf("HandleAgentResponse failed: %v", err)
		}
	}

	// PostMessageWithFeedback sends JSON via aiClient, so the body is parsed as JSON
	for _, call := range *calls {
		if call.Method == "/chat.postMessage" {
			// Re-marshal the body to search for expected keys
			bodyJSON, err := json.Marshal(call.Body)
			if err != nil {
				t.Fatalf("failed to marshal call body: %v", err)
			}
			bodyStr := string(bodyJSON)

			if !strings.Contains(bodyStr, "context_actions") {
				t.Errorf("expected body to contain 'context_actions' block type, got: %s", bodyStr)
			}
			if !strings.Contains(bodyStr, "feedback_buttons") {
				t.Errorf("expected body to contain 'feedback_buttons' element type, got: %s", bodyStr)
			}
			if !strings.Contains(bodyStr, "positive_feedback") {
				t.Errorf("expected body to contain 'positive_feedback' value, got: %s", bodyStr)
			}
			if !strings.Contains(bodyStr, "negative_feedback") {
				t.Errorf("expected body to contain 'negative_feedback' value, got: %s", bodyStr)
			}
			return
		}
	}
	t.Error("expected chat.postMessage call for END chunk")
}

func TestSlackAdapter_HandleAgentResponse_ContentEnd_MessageTextAndBlocks(t *testing.T) {
	a, calls, cleanup := newTestSlackAdapter(t)
	defer cleanup()

	convID := "C123-1234567890.000001"

	for _, resp := range []*pb.AgentResponse{
		{ConversationId: convID, Payload: &pb.AgentResponse_Content{Content: &pb.ContentChunk{Type: pb.ContentChunk_START}}},
		{ConversationId: convID, Payload: &pb.AgentResponse_Content{Content: &pb.ContentChunk{Type: pb.ContentChunk_DELTA, Content: "Test response"}}},
		{ConversationId: convID, Payload: &pb.AgentResponse_Content{Content: &pb.ContentChunk{Type: pb.ContentChunk_END}}},
	} {
		if err := a.HandleAgentResponse(t.Context(), resp); err != nil {
			t.Fatalf("HandleAgentResponse failed: %v", err)
		}
	}

	for _, call := range *calls {
		if call.Method == "/chat.postMessage" {
			// Verify fallback text is set
			text, _ := call.Body["text"].(string)
			if text != "Test response" {
				t.Errorf("expected fallback text 'Test response', got %q", text)
			}

			// Re-marshal to check blocks content
			bodyJSON, _ := json.Marshal(call.Body)
			bodyStr := string(bodyJSON)
			if !strings.Contains(bodyStr, "Test response") {
				t.Errorf("expected body to contain message content, got: %s", bodyStr)
			}
			if !strings.Contains(bodyStr, "section") {
				t.Errorf("expected body to contain section block, got: %s", bodyStr)
			}
			return
		}
	}
	t.Error("expected chat.postMessage call")
}

func TestSlackAdapter_HandleAgentResponse_ErrorUnspecifiedCode(t *testing.T) {
	a, calls, cleanup := newTestSlackAdapter(t)
	defer cleanup()

	response := &pb.AgentResponse{
		ConversationId: "C123-1234567890.000001",
		Payload: &pb.AgentResponse_Error{
			Error: &pb.ErrorResponse{
				Code:    pb.ErrorResponse_ERROR_CODE_UNSPECIFIED,
				Message: "Unknown error",
			},
		},
	}

	err := a.HandleAgentResponse(t.Context(), response)
	if err != nil {
		t.Fatalf("HandleAgentResponse failed: %v", err)
	}

	// Verify the error message does NOT include the code when unspecified
	for _, call := range *calls {
		if call.Method == "/chat.postMessage" {
			text, ok := call.Body["text"].(string)
			if !ok {
				t.Error("expected text field in body")
				continue
			}
			if strings.Contains(text, "ERROR_CODE_UNSPECIFIED") {
				t.Errorf("should not include unspecified error code in message, got: %s", text)
			}
		}
	}
}
