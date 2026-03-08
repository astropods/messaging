package telephony

import (
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type capturedSegment struct {
	conversationID string
	config         AudioConfig
	data           []byte
}

func TestTwilioHandler_FullFlow(t *testing.T) {
	var segments []capturedSegment
	var mu sync.Mutex

	handler := NewTwilioHandler(func(conversationID string, config AudioConfig, data []byte) {
		mu.Lock()
		segments = append(segments, capturedSegment{conversationID, config, append([]byte{}, data...)})
		mu.Unlock()
	})

	server := httptest.NewServer(handler)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	ws, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("WebSocket dial failed: %v", err)
	}
	defer ws.Close()

	// Send connected event
	sendJSON(t, ws, twilioMessage{Event: "connected"})

	// Send start event
	sendJSON(t, ws, twilioMessage{
		Event:     "start",
		StreamSid: "stream-123",
		Start: &twilioStart{
			StreamSid:  "stream-123",
			CallSid:    "call-456",
			AccountSid: "acct-789",
			MediaFormat: twilioMediaFormat{
				Encoding:   "audio/x-mulaw",
				SampleRate: 8000,
				Channels:   1,
			},
			CustomParameters: map[string]string{
				"language": "es-MX",
			},
		},
	})

	// Send media events with base64 audio
	audioChunk1 := []byte{0x01, 0x02, 0x03, 0x04}
	audioChunk2 := []byte{0x05, 0x06, 0x07, 0x08}

	sendJSON(t, ws, twilioMessage{
		Event: "media",
		Media: &twilioMedia{
			Track:   "inbound",
			Chunk:   "1",
			Payload: base64.StdEncoding.EncodeToString(audioChunk1),
		},
	})

	sendJSON(t, ws, twilioMessage{
		Event: "media",
		Media: &twilioMedia{
			Track:   "inbound",
			Chunk:   "2",
			Payload: base64.StdEncoding.EncodeToString(audioChunk2),
		},
	})

	// Send stop event
	sendJSON(t, ws, twilioMessage{Event: "stop"})

	// Give handler time to process
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	if len(segments) != 1 {
		t.Fatalf("expected 1 segment, got %d", len(segments))
	}

	seg := segments[0]
	if seg.conversationID != "twilio-call-456" {
		t.Errorf("expected conversationID 'twilio-call-456', got %q", seg.conversationID)
	}
	if seg.config.Encoding != "mulaw" {
		t.Errorf("expected encoding 'mulaw', got %q", seg.config.Encoding)
	}
	if seg.config.SampleRate != 8000 {
		t.Errorf("expected sampleRate 8000, got %d", seg.config.SampleRate)
	}
	if seg.config.Language != "es-MX" {
		t.Errorf("expected language 'es-MX', got %q", seg.config.Language)
	}
	if seg.config.Source != "twilio" {
		t.Errorf("expected source 'twilio', got %q", seg.config.Source)
	}

	expected := append(audioChunk1, audioChunk2...)
	if len(seg.data) != len(expected) {
		t.Fatalf("expected %d bytes, got %d", len(expected), len(seg.data))
	}
	for i, b := range expected {
		if seg.data[i] != b {
			t.Errorf("byte %d: expected %02x, got %02x", i, b, seg.data[i])
		}
	}
}

func TestTwilioHandler_IgnoresOutboundTrack(t *testing.T) {
	var segments []capturedSegment
	var mu sync.Mutex

	handler := NewTwilioHandler(func(conversationID string, config AudioConfig, data []byte) {
		mu.Lock()
		segments = append(segments, capturedSegment{conversationID, config, append([]byte{}, data...)})
		mu.Unlock()
	})

	server := httptest.NewServer(handler)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	ws, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("WebSocket dial failed: %v", err)
	}
	defer ws.Close()

	sendJSON(t, ws, twilioMessage{
		Event: "start",
		Start: &twilioStart{
			CallSid:     "call-out",
			MediaFormat: twilioMediaFormat{SampleRate: 8000, Channels: 1},
		},
	})

	// Send outbound track (should be ignored)
	sendJSON(t, ws, twilioMessage{
		Event: "media",
		Media: &twilioMedia{
			Track:   "outbound",
			Payload: base64.StdEncoding.EncodeToString([]byte{0xFF}),
		},
	})

	// Send inbound track (should be captured)
	sendJSON(t, ws, twilioMessage{
		Event: "media",
		Media: &twilioMedia{
			Track:   "inbound",
			Payload: base64.StdEncoding.EncodeToString([]byte{0xAA}),
		},
	})

	sendJSON(t, ws, twilioMessage{Event: "stop"})
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	if len(segments) != 1 {
		t.Fatalf("expected 1 segment, got %d", len(segments))
	}
	if len(segments[0].data) != 1 || segments[0].data[0] != 0xAA {
		t.Errorf("expected only inbound data [0xAA], got %v", segments[0].data)
	}
}

func TestTwilioHandler_FlushOnDisconnect(t *testing.T) {
	var segments []capturedSegment
	var mu sync.Mutex

	handler := NewTwilioHandler(func(conversationID string, config AudioConfig, data []byte) {
		mu.Lock()
		segments = append(segments, capturedSegment{conversationID, config, append([]byte{}, data...)})
		mu.Unlock()
	})

	server := httptest.NewServer(handler)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	ws, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("WebSocket dial failed: %v", err)
	}

	sendJSON(t, ws, twilioMessage{
		Event: "start",
		Start: &twilioStart{
			CallSid:     "call-dc",
			MediaFormat: twilioMediaFormat{SampleRate: 8000, Channels: 1},
		},
	})

	sendJSON(t, ws, twilioMessage{
		Event: "media",
		Media: &twilioMedia{
			Track:   "inbound",
			Payload: base64.StdEncoding.EncodeToString([]byte("audio-before-disconnect")),
		},
	})

	// Close without stop event (simulates network drop)
	ws.WriteMessage(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	ws.Close()

	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	if len(segments) != 1 {
		t.Fatalf("expected 1 segment from flush on disconnect, got %d", len(segments))
	}
	if string(segments[0].data) != "audio-before-disconnect" {
		t.Errorf("unexpected data: %s", segments[0].data)
	}
}

func sendJSON(t *testing.T, ws *websocket.Conn, v interface{}) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("failed to marshal JSON: %v", err)
	}
	if err := ws.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatalf("failed to write WebSocket message: %v", err)
	}
}
