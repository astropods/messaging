package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
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

	req := httptest.NewRequest("POST", "/api/conversations/conv-123/audio", &buf)
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

	req := httptest.NewRequest("POST", "/api/conversations/conv-123/audio",
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

	req := httptest.NewRequest("POST", "/api/conversations/conv-456/audio", &buf)
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
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
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
	if pd["audio_size_bytes"] != "2048" {
		t.Errorf("expected size '2048', got %q", pd["audio_size_bytes"])
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
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
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
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("WebSocket dial failed: %v", err)
	}

	// Send config + audio but NO audio.end, then close
	config := AudioConfig{
		Type:       "audio.config",
		Encoding:   "mulaw",
		SampleRate: 8000,
		Channels:   1,
		Source:     "twilio",
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

func TestHandleAudioStream_ServerResponses(t *testing.T) {
	handler := newTestHandlers(func(ctx context.Context, msg *pb.Message) error {
		return nil
	})

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/conversations/{id}/audio", handler.HandleAudioStream)

	server := httptest.NewServer(mux)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/conversations/conv-resp/audio"
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("WebSocket dial failed: %v", err)
	}
	defer ws.Close()

	// Give time for connection to register
	time.Sleep(50 * time.Millisecond)

	// Broadcast a status event to the conversation
	statusEvent := NewStatusEvent(&pb.StatusUpdate{
		Status:        pb.StatusUpdate_THINKING,
		CustomMessage: "Processing audio...",
	})
	handler.connManager.Broadcast("conv-resp", statusEvent)

	// Read the response from WebSocket
	ws.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, data, err := ws.ReadMessage()
	if err != nil {
		t.Fatalf("Failed to read WS response: %v", err)
	}

	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("Failed to parse response: %v", err)
	}
	if string(envelope["event"]) != `"status"` {
		t.Errorf("expected event 'status', got %s", envelope["event"])
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

// Helpers
func readAll(r io.Reader) []byte {
	data, _ := io.ReadAll(r)
	return data
}
