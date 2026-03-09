package web

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/astropods/messaging/internal/adapter"
	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
	"github.com/gorilla/websocket"
)

func newTestHandlers(handler adapter.MessageHandler) *Handlers {
	cm := NewConnectionManager(30 * time.Second)
	h := &Handlers{
		connManager:    cm,
		sessionManager: &NoopSessionManager{},
	}
	h.msgHandler = handler
	return h
}

func TestHandleAudioUpload(t *testing.T) {
	var received *pb.Message
	var mu sync.Mutex

	handler := newTestHandlers(func(ctx context.Context, msg *pb.Message) error {
		mu.Lock()
		received = msg
		mu.Unlock()
		return nil
	})

	// Build multipart request
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	// Add audio file
	part, err := writer.CreateFormFile("audio", "recording.webm")
	if err != nil {
		t.Fatal(err)
	}
	audioData := []byte("fake-audio-data-for-testing")
	part.Write(audioData)

	// Add encoding field
	writer.WriteField("encoding", "webm_opus")
	writer.WriteField("language", "en-US")
	writer.WriteField("source", "browser")
	writer.Close()

	// Use path pattern with {id}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/conversations/{id}/audio", handler.HandleAudioUpload)

	req := httptest.NewRequest(http.MethodPost, "/api/conversations/conv-123/audio", &buf)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()

	mux.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Errorf("expected status 202, got %d: %s", w.Code, w.Body.String())
	}

	// Verify the response JSON
	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp["status"] != "accepted" {
		t.Errorf("expected status 'accepted', got %q", resp["status"])
	}

	// Verify the message was forwarded
	mu.Lock()
	msg := received
	mu.Unlock()

	if msg == nil {
		t.Fatal("expected message to be forwarded, got nil")
	}
	if msg.ConversationId != "conv-123" {
		t.Errorf("expected conversationId 'conv-123', got %q", msg.ConversationId)
	}
	if msg.Content != "[audio]" {
		t.Errorf("expected content '[audio]', got %q", msg.Content)
	}
	if len(msg.Attachments) != 1 {
		t.Fatalf("expected 1 attachment, got %d", len(msg.Attachments))
	}
	att := msg.Attachments[0]
	if att.Type != pb.Attachment_AUDIO {
		t.Errorf("expected AUDIO attachment type, got %v", att.Type)
	}
	if att.MimeType != "audio/webm;codecs=opus" {
		t.Errorf("expected mime 'audio/webm;codecs=opus', got %q", att.MimeType)
	}

	// Verify platform_data has audio metadata
	pd := msg.PlatformContext.PlatformData
	if pd["audio_encoding"] != "webm_opus" {
		t.Errorf("expected audio_encoding 'webm_opus', got %q", pd["audio_encoding"])
	}
	if pd["audio_language"] != "en-US" {
		t.Errorf("expected audio_language 'en-US', got %q", pd["audio_language"])
	}
	if pd["audio_source"] != "browser" {
		t.Errorf("expected audio_source 'browser', got %q", pd["audio_source"])
	}
}

func TestHandleAudioUpload_MissingFile(t *testing.T) {
	handler := newTestHandlers(nil)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/conversations/{id}/audio", handler.HandleAudioUpload)

	req := httptest.NewRequest(http.MethodPost, "/api/conversations/conv-123/audio",
		strings.NewReader("not multipart"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()

	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", w.Code)
	}
}

func TestHandleAudioUpload_InferEncoding(t *testing.T) {
	var received *pb.Message
	var mu sync.Mutex

	handler := newTestHandlers(func(ctx context.Context, msg *pb.Message) error {
		mu.Lock()
		received = msg
		mu.Unlock()
		return nil
	})

	// Build multipart without explicit encoding field
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	// Create form file with content-type header
	h := make(map[string][]string)
	h["Content-Disposition"] = []string{`form-data; name="audio"; filename="voice.ogg"`}
	h["Content-Type"] = []string{"audio/ogg;codecs=opus"}
	part, _ := writer.CreatePart(h)
	part.Write([]byte("fake-ogg-audio"))
	writer.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/conversations/{id}/audio", handler.HandleAudioUpload)

	req := httptest.NewRequest(http.MethodPost, "/api/conversations/conv-456/audio", &buf)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()

	mux.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Errorf("expected status 202, got %d: %s", w.Code, w.Body.String())
	}

	mu.Lock()
	msg := received
	mu.Unlock()

	if msg == nil {
		t.Fatal("expected message to be forwarded")
	}
	pd := msg.PlatformContext.PlatformData
	if pd["audio_encoding"] != "ogg_opus" {
		t.Errorf("expected inferred encoding 'ogg_opus', got %q", pd["audio_encoding"])
	}
}

func TestHandleAudioStream_WebSocket(t *testing.T) {
	var received *pb.Message
	var mu sync.Mutex

	handler := newTestHandlers(func(ctx context.Context, msg *pb.Message) error {
		mu.Lock()
		received = msg
		mu.Unlock()
		return nil
	})

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/conversations/{id}/audio", handler.HandleAudioStream)

	server := httptest.NewServer(mux)
	defer server.Close()

	// Connect WebSocket
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/conversations/conv-ws-1/audio"
	ws, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("WebSocket dial failed: %v", err)
	}
	defer ws.Close()

	// Send audio config
	config := AudioConfig{
		Type:       "audio.config",
		Encoding:   "linear16",
		SampleRate: 16000,
		Channels:   1,
		Language:   "en-US",
		Source:     "browser",
	}
	configJSON, _ := json.Marshal(config)
	if err := ws.WriteMessage(websocket.TextMessage, configJSON); err != nil {
		t.Fatalf("Failed to send config: %v", err)
	}

	// Send some audio data
	audioData := make([]byte, 1024)
	for i := range audioData {
		audioData[i] = byte(i % 256)
	}
	if err := ws.WriteMessage(websocket.BinaryMessage, audioData); err != nil {
		t.Fatalf("Failed to send audio: %v", err)
	}

	// Send more audio
	if err := ws.WriteMessage(websocket.BinaryMessage, audioData); err != nil {
		t.Fatalf("Failed to send audio 2: %v", err)
	}

	// End segment
	endMsg, _ := json.Marshal(AudioControl{Type: "audio.end"})
	if err := ws.WriteMessage(websocket.TextMessage, endMsg); err != nil {
		t.Fatalf("Failed to send audio.end: %v", err)
	}

	// Give handler time to process
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	msg := received
	mu.Unlock()

	if msg == nil {
		t.Fatal("expected message to be forwarded after audio.end")
	}
	if msg.ConversationId != "conv-ws-1" {
		t.Errorf("expected conversationId 'conv-ws-1', got %q", msg.ConversationId)
	}
	if msg.Content != "[audio]" {
		t.Errorf("expected content '[audio]', got %q", msg.Content)
	}

	pd := msg.PlatformContext.PlatformData
	if pd["audio_encoding"] != "linear16" {
		t.Errorf("expected encoding 'linear16', got %q", pd["audio_encoding"])
	}
}

func TestHandleAudioStream_MultipleSegments(t *testing.T) {
	var messages []*pb.Message
	var mu sync.Mutex

	handler := newTestHandlers(func(ctx context.Context, msg *pb.Message) error {
		mu.Lock()
		messages = append(messages, msg)
		mu.Unlock()
		return nil
	})

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/conversations/{id}/audio", handler.HandleAudioStream)

	server := httptest.NewServer(mux)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/conversations/conv-multi/audio"
	ws, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("WebSocket dial failed: %v", err)
	}
	defer ws.Close()

	// Send two segments on the same connection
	for i := 0; i < 2; i++ {
		config := AudioConfig{
			Type:       "audio.config",
			Encoding:   "webm_opus",
			SampleRate: 48000,
			Channels:   1,
			Source:     "browser",
		}
		configJSON, _ := json.Marshal(config)
		ws.WriteMessage(websocket.TextMessage, configJSON)
		ws.WriteMessage(websocket.BinaryMessage, []byte("audio-segment-data"))

		endMsg, _ := json.Marshal(AudioControl{Type: "audio.end"})
		ws.WriteMessage(websocket.TextMessage, endMsg)

		time.Sleep(50 * time.Millisecond)
	}

	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	count := len(messages)
	mu.Unlock()

	if count != 2 {
		t.Errorf("expected 2 messages from 2 segments, got %d", count)
	}
}

func TestHandleAudioStream_FlushOnClose(t *testing.T) {
	var received *pb.Message
	var mu sync.Mutex

	handler := newTestHandlers(func(ctx context.Context, msg *pb.Message) error {
		mu.Lock()
		received = msg
		mu.Unlock()
		return nil
	})

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/conversations/{id}/audio", handler.HandleAudioStream)

	server := httptest.NewServer(mux)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/conversations/conv-flush/audio"
	ws, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("WebSocket dial failed: %v", err)
	}

	// Send config + audio but NO audio.end, then close
	config := AudioConfig{
		Type:       "audio.config",
		Encoding:   "mulaw",
		SampleRate: 8000,
		Channels:   1,
		Source:     "browser",
	}
	configJSON, _ := json.Marshal(config)
	ws.WriteMessage(websocket.TextMessage, configJSON)
	ws.WriteMessage(websocket.BinaryMessage, []byte("buffered-audio"))

	// Close without sending audio.end
	ws.WriteMessage(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	ws.Close()

	// Give handler time to flush
	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	msg := received
	mu.Unlock()

	if msg == nil {
		t.Fatal("expected buffered audio to be flushed on close")
	}
	if msg.PlatformContext.PlatformData["audio_encoding"] != "mulaw" {
		t.Errorf("expected encoding 'mulaw', got %q", msg.PlatformContext.PlatformData["audio_encoding"])
	}
}

func TestHandleAudioStream_DoesNotReceiveBroadcasts(t *testing.T) {
	handler := newTestHandlers(func(ctx context.Context, msg *pb.Message) error {
		return nil
	})

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/conversations/{id}/audio", handler.HandleAudioStream)

	server := httptest.NewServer(mux)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/conversations/conv-resp/audio"
	ws, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("WebSocket dial failed: %v", err)
	}
	defer ws.Close()

	// Give time for handler to start
	time.Sleep(50 * time.Millisecond)

	// Broadcast a status event to the conversation — audio WS should NOT receive it
	statusEvent := NewStatusEvent(&pb.StatusUpdate{
		Status:        pb.StatusUpdate_THINKING,
		CustomMessage: "Processing audio...",
	})
	handler.connManager.Broadcast("conv-resp", statusEvent)

	// Attempt to read — should time out because the audio WS is ingest-only
	ws.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	_, _, err = ws.ReadMessage()
	if err == nil {
		t.Fatal("expected no message on audio WS, but received one — audio WS should be ingest-only")
	}
}

func TestHandleAudioStream_ResponsesGoToSSE(t *testing.T) {
	handler := newTestHandlers(func(ctx context.Context, msg *pb.Message) error {
		return nil
	})

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/conversations/{id}/audio", handler.HandleAudioStream)

	server := httptest.NewServer(mux)
	defer server.Close()

	// Open audio WS (ingest-only)
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/conversations/conv-sse/audio"
	ws, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("WebSocket dial failed: %v", err)
	}
	defer ws.Close()

	// Register a regular SSE connection for the same conversation
	sseConn := &SSEConnection{
		ID:             "sse-test",
		ConversationID: "conv-sse",
		EventChan:      make(chan SSEEvent, 10),
		Done:           make(chan struct{}),
	}
	handler.connManager.Add(sseConn)
	defer handler.connManager.Remove("conv-sse", sseConn.ID)

	// Broadcast an event
	statusEvent := NewStatusEvent(&pb.StatusUpdate{
		Status:        pb.StatusUpdate_THINKING,
		CustomMessage: "Thinking...",
	})
	handler.connManager.Broadcast("conv-sse", statusEvent)

	// SSE connection should receive the event
	select {
	case event := <-sseConn.EventChan:
		if event.Event != EventStatus {
			t.Errorf("expected status event, got %q", event.Event)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("SSE connection did not receive broadcast event")
	}
}

func TestEncodingToExtension(t *testing.T) {
	tests := []struct {
		encoding string
		expected string
	}{
		{"webm_opus", "webm"},
		{"ogg_opus", "ogg"},
		{"linear16", "wav"},
		{"mulaw", "wav"},
		{"opus", "opus"},
		{"mp3", "mp3"},
		{"flac", "flac"},
		{"aac", "m4a"},
		{"unknown", "bin"},
	}
	for _, tt := range tests {
		got := encodingToExtension(tt.encoding)
		if got != tt.expected {
			t.Errorf("encodingToExtension(%q) = %q, want %q", tt.encoding, got, tt.expected)
		}
	}
}

func TestMimeToEncoding(t *testing.T) {
	tests := []struct {
		mime     string
		expected string
	}{
		{"audio/webm", "webm_opus"},
		{"audio/webm;codecs=opus", "webm_opus"},
		{"audio/ogg", "ogg_opus"},
		{"audio/mpeg", "mp3"},
		{"audio/flac", "flac"},
		{"audio/aac", "aac"},
		{"audio/wav", "linear16"},
		{"audio/opus", "opus"},
		{"application/octet-stream", "webm_opus"}, // default
	}
	for _, tt := range tests {
		got := mimeToEncoding(tt.mime)
		if got != tt.expected {
			t.Errorf("mimeToEncoding(%q) = %q, want %q", tt.mime, got, tt.expected)
		}
	}
}

func TestEncodingToMIME(t *testing.T) {
	tests := []struct {
		encoding string
		expected string
	}{
		{"webm_opus", "audio/webm;codecs=opus"},
		{"mulaw", "audio/basic"},
		{"linear16", "audio/L16"},
		{"mp3", "audio/mpeg"},
		{"unknown", ""},
	}
	for _, tt := range tests {
		got := encodingToMIME[tt.encoding]
		if got != tt.expected {
			t.Errorf("encodingToMIME[%q] = %q, want %q", tt.encoding, got, tt.expected)
		}
	}
}

// --- mockAudioForwarder captures SendAudioConfig/SendAudioChunk calls ---

type audioForwarderCall struct {
	method         string // "config" or "chunk"
	conversationID string
	config         *pb.AudioStreamConfig
	data           []byte
	sequence       int64
	done           bool
}

type mockAudioForwarder struct {
	mu    sync.Mutex
	calls []audioForwarderCall
}

func (m *mockAudioForwarder) SendAudioConfig(conversationID string, config *pb.AudioStreamConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, audioForwarderCall{
		method:         "config",
		conversationID: conversationID,
		config:         config,
	})
	return nil
}

func (m *mockAudioForwarder) SendAudioChunk(conversationID string, data []byte, sequence int64, done bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, audioForwarderCall{
		method:         "chunk",
		conversationID: conversationID,
		data:           data,
		sequence:       sequence,
		done:           done,
	})
	return nil
}

func (m *mockAudioForwarder) getCalls() []audioForwarderCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]audioForwarderCall, len(m.calls))
	copy(cp, m.calls)
	return cp
}

func TestHandleAudioUpload_ConsistentMessageID(t *testing.T) {
	var receivedMsg *pb.Message
	var mu sync.Mutex

	fwd := &mockAudioForwarder{}
	handler := newTestHandlers(func(ctx context.Context, msg *pb.Message) error {
		mu.Lock()
		receivedMsg = msg
		mu.Unlock()
		return nil
	})
	handler.audioForwarder = fwd

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	part, _ := writer.CreateFormFile("audio", "recording.webm")
	part.Write([]byte("fake-audio"))
	writer.WriteField("encoding", "webm_opus")
	writer.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/conversations/{id}/audio", handler.HandleAudioUpload)

	req := httptest.NewRequest(http.MethodPost, "/api/conversations/conv-id-test/audio", &buf)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()

	mux.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
	}

	// Get the message ID the agent received
	mu.Lock()
	agentMessageID := receivedMsg.Id
	mu.Unlock()

	// Get the message ID from the HTTP response
	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	clientMessageID := resp["message_id"]

	if agentMessageID == "" {
		t.Fatal("agent message ID should not be empty")
	}
	if clientMessageID == "" {
		t.Fatal("client message ID should not be empty")
	}
	if agentMessageID != clientMessageID {
		t.Errorf("message ID mismatch: agent got %q, client got %q", agentMessageID, clientMessageID)
	}
}

func TestHandleAudioUpload_ConfigBeforeMessage(t *testing.T) {
	// Track the order of operations: audio config should be sent before message metadata
	var callOrder []string
	var mu sync.Mutex

	fwd := &mockAudioForwarder{}

	handler := newTestHandlers(func(ctx context.Context, msg *pb.Message) error {
		mu.Lock()
		callOrder = append(callOrder, "message")
		mu.Unlock()
		return nil
	})
	handler.audioForwarder = fwd

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	part, _ := writer.CreateFormFile("audio", "recording.webm")
	part.Write([]byte("audio-data"))
	writer.WriteField("encoding", "webm_opus")
	writer.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/conversations/{id}/audio", handler.HandleAudioUpload)

	req := httptest.NewRequest(http.MethodPost, "/api/conversations/conv-order/audio", &buf)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()

	mux.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", w.Code)
	}

	// Check the forwarder calls: config should come first
	calls := fwd.getCalls()
	if len(calls) < 2 {
		t.Fatalf("expected at least 2 forwarder calls, got %d", len(calls))
	}
	if calls[0].method != "config" {
		t.Errorf("first forwarder call should be 'config', got %q", calls[0].method)
	}

	// Message handler should be called after the config
	mu.Lock()
	order := callOrder
	mu.Unlock()

	if len(order) != 1 || order[0] != "message" {
		t.Errorf("expected message handler to be called once, got: %v", order)
	}

	// The audio chunk (done=true) should come after message
	if calls[1].method != "chunk" || !calls[1].done {
		t.Errorf("second forwarder call should be chunk with done=true, got method=%q done=%v",
			calls[1].method, calls[1].done)
	}
}

func TestHandleAudioStream_ConfigBeforeMessage(t *testing.T) {
	// Track ordering: for WebSocket streaming, audio config should be sent before message metadata
	var messageReceived bool
	var mu sync.Mutex

	fwd := &mockAudioForwarder{}

	handler := newTestHandlers(func(ctx context.Context, msg *pb.Message) error {
		mu.Lock()
		messageReceived = true
		mu.Unlock()
		return nil
	})
	handler.audioForwarder = fwd

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/conversations/{id}/audio", handler.HandleAudioStream)

	server := httptest.NewServer(mux)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/conversations/conv-ws-order/audio"
	ws, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("WebSocket dial failed: %v", err)
	}
	defer ws.Close()

	// Send config
	config := AudioConfig{
		Type:       "audio.config",
		Encoding:   "linear16",
		SampleRate: 16000,
		Channels:   1,
		Source:     "browser",
	}
	configJSON, _ := json.Marshal(config)
	ws.WriteMessage(websocket.TextMessage, configJSON)

	// Give handler time to process
	time.Sleep(100 * time.Millisecond)

	// Verify: audio config was sent to forwarder BEFORE message handler was called
	calls := fwd.getCalls()
	if len(calls) < 1 {
		t.Fatal("expected at least 1 forwarder call for audio config")
	}
	if calls[0].method != "config" {
		t.Errorf("first forwarder call should be 'config', got %q", calls[0].method)
	}
	if calls[0].config.Encoding != pb.AudioEncoding_LINEAR16 {
		t.Errorf("expected LINEAR16 encoding, got %v", calls[0].config.Encoding)
	}

	mu.Lock()
	gotMessage := messageReceived
	mu.Unlock()

	if !gotMessage {
		t.Error("expected message handler to be called after audio config")
	}
}

func TestHandleAudioStream_ForwardsChunksToAgent(t *testing.T) {
	fwd := &mockAudioForwarder{}

	handler := newTestHandlers(func(ctx context.Context, msg *pb.Message) error {
		return nil
	})
	handler.audioForwarder = fwd

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/conversations/{id}/audio", handler.HandleAudioStream)

	server := httptest.NewServer(mux)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/conversations/conv-fwd/audio"
	ws, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("WebSocket dial failed: %v", err)
	}
	defer ws.Close()

	// Send config
	config := AudioConfig{Type: "audio.config", Encoding: "webm_opus", SampleRate: 48000, Channels: 1}
	configJSON, _ := json.Marshal(config)
	ws.WriteMessage(websocket.TextMessage, configJSON)

	// Send binary audio chunks
	ws.WriteMessage(websocket.BinaryMessage, []byte{0xAA, 0xBB})
	ws.WriteMessage(websocket.BinaryMessage, []byte{0xCC, 0xDD})

	// End segment
	endMsg, _ := json.Marshal(AudioControl{Type: "audio.end"})
	ws.WriteMessage(websocket.TextMessage, endMsg)

	time.Sleep(100 * time.Millisecond)

	calls := fwd.getCalls()

	// Expect: config, chunk(seq=1), chunk(seq=2), chunk(done=true)
	if len(calls) < 4 {
		t.Fatalf("expected at least 4 forwarder calls, got %d", len(calls))
	}

	if calls[0].method != "config" {
		t.Errorf("call[0] should be config, got %q", calls[0].method)
	}
	if calls[1].method != "chunk" || calls[1].sequence != 1 {
		t.Errorf("call[1] should be chunk seq=1, got method=%q seq=%d", calls[1].method, calls[1].sequence)
	}
	if calls[2].method != "chunk" || calls[2].sequence != 2 {
		t.Errorf("call[2] should be chunk seq=2, got method=%q seq=%d", calls[2].method, calls[2].sequence)
	}
	if calls[3].method != "chunk" || !calls[3].done {
		t.Errorf("call[3] should be chunk done=true, got method=%q done=%v", calls[3].method, calls[3].done)
	}
}

