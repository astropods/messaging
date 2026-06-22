package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

func TestSlackAIClient_PostMessageWithFeedback_DevMode(t *testing.T) {
	var capturedBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&capturedBody)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "9999.000001"})
	}))
	defer server.Close()

	client := &SlackAIClient{
		botToken:   "xoxb-test-token",
		devMode:    true,
		httpClient: server.Client(),
		baseURL:    server.URL,
	}

	_, err := client.PostMessageWithFeedback(context.Background(), "C123", "Hello", "1234.000001")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	blocks, ok := capturedBody["blocks"].([]any)
	if !ok {
		t.Fatal("expected blocks in payload")
	}

	// Find the dev context block (should be between content and feedback buttons)
	foundDevContext := false
	for _, b := range blocks {
		block, ok := b.(map[string]any)
		if !ok {
			continue
		}
		if block["type"] == "context" {
			elements, ok := block["elements"].([]any)
			if !ok || len(elements) == 0 {
				continue
			}
			elem, ok := elements[0].(map[string]any)
			if !ok {
				continue
			}
			if text, ok := elem["text"].(string); ok && strings.Contains(text, "dev environment") {
				foundDevContext = true
			}
		}
	}
	if !foundDevContext {
		t.Error("expected a context block with dev environment indicator")
	}
}

// firstContextFooter returns the mrkdwn text of the first context block
// found in the captured Slack payload, or "" if none.
func firstContextFooter(t *testing.T, capturedBody map[string]any) string {
	t.Helper()
	blocks, ok := capturedBody["blocks"].([]any)
	if !ok {
		return ""
	}
	for _, b := range blocks {
		block, ok := b.(map[string]any)
		if !ok || block["type"] != "context" {
			continue
		}
		elements, ok := block["elements"].([]any)
		if !ok || len(elements) == 0 {
			continue
		}
		elem, ok := elements[0].(map[string]any)
		if !ok {
			continue
		}
		if text, ok := elem["text"].(string); ok {
			return text
		}
	}
	return ""
}

func TestSlackAIClient_PostMessageWithFeedback_DevMode_WithAgentID(t *testing.T) {
	var capturedBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&capturedBody)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "9999.000001"})
	}))
	defer server.Close()

	client := &SlackAIClient{
		botToken:   "xoxb-test-token",
		devMode:    true,
		agentID:    "agent-xyz-123",
		httpClient: server.Client(),
		baseURL:    server.URL,
	}

	_, err := client.PostMessageWithFeedback(context.Background(), "C123", "Hello", "1234.000001")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	text := firstContextFooter(t, capturedBody)
	if !strings.Contains(text, "dev environment") || !strings.Contains(text, "agent-xyz-123") {
		t.Errorf("expected dev footer to include agent ID, got %q", text)
	}
}

func TestSlackAIClient_PostMessageWithFeedback_NoDevMode_WithAgentID(t *testing.T) {
	var capturedBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&capturedBody)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "9999.000001"})
	}))
	defer server.Close()

	client := &SlackAIClient{
		botToken:   "xoxb-test-token",
		agentID:    "agent-xyz-123",
		httpClient: server.Client(),
		baseURL:    server.URL,
	}

	_, err := client.PostMessageWithFeedback(context.Background(), "C123", "Hello", "1234.000001")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	text := firstContextFooter(t, capturedBody)
	if !strings.Contains(text, "Agent ID:") || !strings.Contains(text, "agent-xyz-123") {
		t.Errorf("expected non-dev footer to be 'Agent ID: <id>', got %q", text)
	}
	if strings.Contains(text, "dev environment") {
		t.Errorf("non-dev footer should not mention dev environment, got %q", text)
	}
}

func TestSlackAIClient_PostMessageWithFeedback_NoDevMode(t *testing.T) {
	var capturedBody map[string]any
	client, cleanup := newTestAIClient(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&capturedBody)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "9999.000001"})
	})
	defer cleanup()

	_, err := client.PostMessageWithFeedback(context.Background(), "C123", "Hello", "1234.000001")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	blocks, ok := capturedBody["blocks"].([]any)
	if !ok {
		t.Fatal("expected blocks in payload")
	}

	for _, b := range blocks {
		block, ok := b.(map[string]any)
		if !ok {
			continue
		}
		if block["type"] == "context" {
			t.Error("should not have a context block when devMode is false")
		}
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

// A reply longer than the per-block limit fans out across multiple threaded
// messages, each carrying one markdown block; the feedback widgets ride only on
// the final message and no content is dropped.
func TestSlackAIClient_PostMessageWithFeedback_FansOutLongReply(t *testing.T) {
	var (
		bodies []map[string]any
		mu     sync.Mutex
	)
	client, cleanup := newTestAIClient(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "9999.000001"})
	})
	defer cleanup()

	// ~40 lines of ~600 chars → well over the 10k per-block limit, forcing a
	// fan-out. Each line is tagged so we can confirm none is dropped.
	const numLines = 40
	lines := make([]string, numLines)
	for i := range lines {
		lines[i] = fmt.Sprintf("L%02d-", i) + strings.Repeat("x", 600)
	}
	content := strings.Join(lines, "\n")

	if _, err := client.PostMessageWithFeedback(context.Background(), "C123", content, "1234.000001"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) < 2 {
		t.Fatalf("expected the long reply to fan out across multiple messages, got %d", len(bodies))
	}

	var allText strings.Builder
	for i, body := range bodies {
		if body["thread_ts"] != "1234.000001" {
			t.Errorf("message %d: expected thread_ts to be set", i)
		}
		blocks, _ := body["blocks"].([]any)
		if len(blocks) == 0 {
			t.Fatalf("message %d: no blocks", i)
		}
		// The first block is always the markdown content block.
		md, _ := blocks[0].(map[string]any)
		if md["type"] != "markdown" {
			t.Errorf("message %d: expected first block to be markdown, got %v", i, md["type"])
		}
		if s, ok := md["text"].(string); ok {
			allText.WriteString(s)
			if len(s) > maxMarkdownBlockChars {
				t.Errorf("message %d: markdown block is %d chars, exceeds cap %d", i, len(s), maxMarkdownBlockChars)
			}
		}
		// Feedback widgets ride only on the last message.
		hasFeedback := len(blocks) > 1
		if isLast := i == len(bodies)-1; hasFeedback != isLast {
			t.Errorf("message %d: feedback present=%v, want %v", i, hasFeedback, isLast)
		}
	}
	for i := range lines {
		tag := fmt.Sprintf("L%02d-", i)
		if !strings.Contains(allText.String(), tag) {
			t.Errorf("content line %s was dropped from the fanned-out reply", tag)
		}
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

// --- Tests for splitIntoChunks ---

func TestSplitIntoChunks_ShortText(t *testing.T) {
	chunks := splitIntoChunks("hello", 100)
	if len(chunks) != 1 || chunks[0] != "hello" {
		t.Errorf("short text should return single chunk, got: %v", chunks)
	}
}

func TestSplitIntoChunks_ExactLimit(t *testing.T) {
	text := strings.Repeat("a", 100)
	chunks := splitIntoChunks(text, 100)
	if len(chunks) != 1 || chunks[0] != text {
		t.Errorf("text at exact limit should return single chunk, got %d chunks", len(chunks))
	}
}

func TestSplitIntoChunks_SplitsOnNewline(t *testing.T) {
	text := "line1\nline2\nline3\nline4"
	chunks := splitIntoChunks(text, 12)

	for _, chunk := range chunks {
		if len(chunk) > 12 {
			t.Errorf("chunk exceeds maxLen: %q (%d chars)", chunk, len(chunk))
		}
	}

	rejoined := strings.Join(chunks, "\n")
	if rejoined != text {
		t.Errorf("rejoined chunks should equal original\ngot:  %q\nwant: %q", rejoined, text)
	}
}

func TestSplitIntoChunks_NoNewlines(t *testing.T) {
	text := strings.Repeat("a", 250)
	chunks := splitIntoChunks(text, 100)

	if len(chunks) != 3 {
		t.Errorf("expected 3 chunks, got %d", len(chunks))
	}
	for _, chunk := range chunks {
		if len(chunk) > 100 {
			t.Errorf("chunk exceeds maxLen: %d chars", len(chunk))
		}
	}

	total := 0
	for _, c := range chunks {
		total += len(c)
	}
	if total != 250 {
		t.Errorf("total characters should be 250, got %d", total)
	}
}

func TestSplitIntoChunks_NoEmptyChunks(t *testing.T) {
	text := "\nline1\n\nline2\n"
	chunks := splitIntoChunks(text, 5)

	for i, chunk := range chunks {
		if chunk == "" {
			t.Errorf("chunk %d is empty", i)
		}
	}
}

func TestSplitIntoChunks_LeadingNewline(t *testing.T) {
	text := "\nabcdef"
	chunks := splitIntoChunks(text, 4)

	for i, chunk := range chunks {
		if chunk == "" {
			t.Errorf("chunk %d is empty", i)
		}
	}

	total := 0
	for _, c := range chunks {
		total += len(c)
	}
	if total < 6 {
		t.Errorf("content should be preserved, total chars: %d", total)
	}
}

// --- Tests for NewSlackAIClient ---

func TestNewSlackAIClient_Defaults(t *testing.T) {
	client := NewSlackAIClient("xoxb-my-token", false, "agent-9")
	if client.botToken != "xoxb-my-token" {
		t.Errorf("expected botToken 'xoxb-my-token', got %q", client.botToken)
	}
	if client.devMode {
		t.Error("expected devMode to be false")
	}
	if client.agentID != "agent-9" {
		t.Errorf("expected agentID 'agent-9', got %q", client.agentID)
	}
	if client.baseURL != slackAPIBaseURL {
		t.Errorf("expected baseURL %q, got %q", slackAPIBaseURL, client.baseURL)
	}
	if client.httpClient == nil {
		t.Error("expected httpClient to be non-nil")
	}
}
