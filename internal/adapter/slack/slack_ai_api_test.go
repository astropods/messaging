package slack

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newTestAIClient creates a SlackAIClient pointing at a mock server.
// The handler func is called for each request.
func newTestAIClient(handler http.HandlerFunc) (*SlackAIClient, func()) {
	server := httptest.NewServer(handler)
	client := &SlackAIClient{
		botToken:   "xoxb-test-token",
		httpClient: server.Client(),
		baseURL:    server.URL,
	}
	return client, server.Close
}

// --- Tests for SetThreadStatus ---

func TestSlackAIClient_SetThreadStatus_Success(t *testing.T) {
	var capturedBody map[string]any
	client, cleanup := newTestAIClient(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/assistant.threads.setStatus") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer xoxb-test-token" {
			t.Errorf("missing or wrong Authorization header: %s", r.Header.Get("Authorization"))
		}
		json.NewDecoder(r.Body).Decode(&capturedBody)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	defer cleanup()

	err := client.SetThreadStatus(context.Background(), "C123", "1234.000001", "Thinking...", ":brain:")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if capturedBody["channel_id"] != "C123" {
		t.Errorf("expected channel_id 'C123', got %v", capturedBody["channel_id"])
	}
	if capturedBody["thread_ts"] != "1234.000001" {
		t.Errorf("expected thread_ts '1234.000001', got %v", capturedBody["thread_ts"])
	}
	if capturedBody["status"] != "Thinking..." {
		t.Errorf("expected status 'Thinking...', got %v", capturedBody["status"])
	}
	if capturedBody["status_emoji"] != ":brain:" {
		t.Errorf("expected status_emoji ':brain:', got %v", capturedBody["status_emoji"])
	}
}

func TestSlackAIClient_SetThreadStatus_NoEmoji(t *testing.T) {
	var capturedBody map[string]any
	client, cleanup := newTestAIClient(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&capturedBody)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	defer cleanup()

	err := client.SetThreadStatus(context.Background(), "C123", "1234.000001", "Working...", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, exists := capturedBody["status_emoji"]; exists {
		t.Error("status_emoji should not be set when emoji is empty")
	}
}

func TestSlackAIClient_SetThreadStatus_APIError(t *testing.T) {
	client, cleanup := newTestAIClient(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"ok":    false,
			"error": "channel_not_found",
		})
	})
	defer cleanup()

	err := client.SetThreadStatus(context.Background(), "C999", "1234.000001", "Thinking...", "")
	if err == nil {
		t.Fatal("expected error for API error response")
	}
	if !strings.Contains(err.Error(), "channel_not_found") {
		t.Errorf("expected error to contain 'channel_not_found', got: %v", err)
	}
}

func TestSlackAIClient_SetThreadStatus_HTTPError(t *testing.T) {
	client, cleanup := newTestAIClient(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal server error"))
	})
	defer cleanup()

	err := client.SetThreadStatus(context.Background(), "C123", "1234.000001", "Thinking...", "")
	if err == nil {
		t.Fatal("expected error for HTTP 500")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("expected error to mention status code 500, got: %v", err)
	}
}

func TestSlackAIClient_SetThreadStatus_ContextCancelled(t *testing.T) {
	client, cleanup := newTestAIClient(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Second) // simulate slow response
	})
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	err := client.SetThreadStatus(ctx, "C123", "1234.000001", "Thinking...", "")
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
}

// --- Tests for SetSuggestedPrompts ---

func TestSlackAIClient_SetSuggestedPrompts_Success(t *testing.T) {
	var capturedBody map[string]any
	client, cleanup := newTestAIClient(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/assistant.threads.setSuggestedPrompts") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&capturedBody)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	defer cleanup()

	prompts := []SuggestedPrompt{
		{Title: "Help", Message: "How can I help?"},
		{Title: "Status", Message: "What's the status?"},
	}

	err := client.SetSuggestedPrompts(context.Background(), "C123", "1234.000001", prompts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if capturedBody["channel_id"] != "C123" {
		t.Errorf("expected channel_id 'C123', got %v", capturedBody["channel_id"])
	}
	promptsArr, ok := capturedBody["prompts"].([]any)
	if !ok || len(promptsArr) != 2 {
		t.Errorf("expected 2 prompts, got %v", capturedBody["prompts"])
	}
}

func TestSlackAIClient_SetSuggestedPrompts_APIError(t *testing.T) {
	client, cleanup := newTestAIClient(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"ok":    false,
			"error": "not_allowed",
		})
	})
	defer cleanup()

	err := client.SetSuggestedPrompts(context.Background(), "C123", "1234.000001", []SuggestedPrompt{
		{Title: "Help", Message: "How can I help?"},
	})
	if err == nil {
		t.Fatal("expected error for API error response")
	}
	if !strings.Contains(err.Error(), "not_allowed") {
		t.Errorf("expected error to contain 'not_allowed', got: %v", err)
	}
}

// --- Tests for SetTitle ---

func TestSlackAIClient_SetTitle_Success(t *testing.T) {
	var capturedBody map[string]any
	client, cleanup := newTestAIClient(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/assistant.threads.setTitle") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&capturedBody)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	defer cleanup()

	err := client.SetTitle(context.Background(), "C123", "1234.000001", "My Thread Title")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if capturedBody["title"] != "My Thread Title" {
		t.Errorf("expected title 'My Thread Title', got %v", capturedBody["title"])
	}
}

func TestSlackAIClient_SetTitle_APIError(t *testing.T) {
	client, cleanup := newTestAIClient(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"ok":    false,
			"error": "thread_not_found",
		})
	})
	defer cleanup()

	err := client.SetTitle(context.Background(), "C123", "1234.000001", "Title")
	if err == nil {
		t.Fatal("expected error for API error response")
	}
	if !strings.Contains(err.Error(), "thread_not_found") {
		t.Errorf("expected error to contain 'thread_not_found', got: %v", err)
	}
}

// --- Tests for PostMessageWithFeedback ---

func TestSlackAIClient_PostMessageWithFeedback_Success(t *testing.T) {
	var capturedBody map[string]any
	client, cleanup := newTestAIClient(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat.postMessage") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&capturedBody)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"ts": "9999.000001",
		})
	})
	defer cleanup()

	ts, err := client.PostMessageWithFeedback(context.Background(), "C123", "Hello world", "1234.000001")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ts != "9999.000001" {
		t.Errorf("expected timestamp '9999.000001', got %q", ts)
	}

	if capturedBody["channel"] != "C123" {
		t.Errorf("expected channel 'C123', got %v", capturedBody["channel"])
	}
	if capturedBody["text"] != "Hello world" {
		t.Errorf("expected text 'Hello world', got %v", capturedBody["text"])
	}
	if capturedBody["thread_ts"] != "1234.000001" {
		t.Errorf("expected thread_ts '1234.000001', got %v", capturedBody["thread_ts"])
	}
	// Verify blocks were included (feedback buttons)
	blocks, ok := capturedBody["blocks"].([]any)
	if !ok || len(blocks) < 2 {
		t.Errorf("expected at least 2 blocks (content + feedback), got %v", capturedBody["blocks"])
	}
}

func TestSlackAIClient_PostMessageWithFeedback_NoThreadID(t *testing.T) {
	var capturedBody map[string]any
	client, cleanup := newTestAIClient(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&capturedBody)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "9999.000001"})
	})
	defer cleanup()

	_, err := client.PostMessageWithFeedback(context.Background(), "C123", "Hello", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, exists := capturedBody["thread_ts"]; exists {
		t.Error("thread_ts should not be set when threadID is empty")
	}
}

func TestSlackAIClient_PostMessageWithFeedback_APIError(t *testing.T) {
	client, cleanup := newTestAIClient(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"ok":    false,
			"error": "too_many_attachments",
		})
	})
	defer cleanup()

	_, err := client.PostMessageWithFeedback(context.Background(), "C123", "Hello", "1234.000001")
	if err == nil {
		t.Fatal("expected error for API error response")
	}
	if !strings.Contains(err.Error(), "too_many_attachments") {
		t.Errorf("expected error to contain 'too_many_attachments', got: %v", err)
	}
}

// --- Tests for postJSON error paths ---

func TestSlackAIClient_postJSON_InvalidJSON(t *testing.T) {
	client, cleanup := newTestAIClient(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("this is not json"))
	})
	defer cleanup()

	var result struct {
		OK bool `json:"ok"`
	}
	err := client.postJSON(context.Background(), "test.method", map[string]any{"key": "val"}, &result)
	if err == nil {
		t.Fatal("expected error for invalid JSON response")
	}
	if !strings.Contains(err.Error(), "unmarshal") {
		t.Errorf("expected unmarshal error, got: %v", err)
	}
}

func TestSlackAIClient_postJSON_HTTP429(t *testing.T) {
	client, cleanup := newTestAIClient(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte("rate limited"))
	})
	defer cleanup()

	var result struct {
		OK bool `json:"ok"`
	}
	err := client.postJSON(context.Background(), "test.method", map[string]any{}, &result)
	if err == nil {
		t.Fatal("expected error for HTTP 429")
	}
	if !strings.Contains(err.Error(), "429") {
		t.Errorf("expected error to mention 429, got: %v", err)
	}
}

func TestSlackAIClient_postJSON_NetworkError(t *testing.T) {
	client := &SlackAIClient{
		botToken:   "xoxb-test-token",
		httpClient: &http.Client{},
		baseURL:    "http://127.0.0.1:1", // nothing listening
	}

	var result struct {
		OK bool `json:"ok"`
	}
	err := client.postJSON(context.Background(), "test.method", map[string]any{}, &result)
	if err == nil {
		t.Fatal("expected error for network failure")
	}
	if !strings.Contains(err.Error(), "request failed") {
		t.Errorf("expected 'request failed' error, got: %v", err)
	}
}

func TestSlackAIClient_postJSON_SetsHeaders(t *testing.T) {
	var capturedHeaders http.Header
	client, cleanup := newTestAIClient(func(w http.ResponseWriter, r *http.Request) {
		capturedHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	defer cleanup()

	var result struct {
		OK bool `json:"ok"`
	}
	client.postJSON(context.Background(), "test.method", map[string]any{}, &result)

	if ct := capturedHeaders.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("expected Content-Type with application/json, got %q", ct)
	}
	if auth := capturedHeaders.Get("Authorization"); auth != "Bearer xoxb-test-token" {
		t.Errorf("expected Authorization 'Bearer xoxb-test-token', got %q", auth)
	}
}

// --- Tests for NewSlackAIClient ---

func TestNewSlackAIClient_Defaults(t *testing.T) {
	client := NewSlackAIClient("xoxb-my-token")
	if client.botToken != "xoxb-my-token" {
		t.Errorf("expected botToken 'xoxb-my-token', got %q", client.botToken)
	}
	if client.baseURL != slackAPIBaseURL {
		t.Errorf("expected baseURL %q, got %q", slackAPIBaseURL, client.baseURL)
	}
	if client.httpClient == nil {
		t.Error("expected httpClient to be non-nil")
	}
}
