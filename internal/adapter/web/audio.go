package web

import (
	"encoding/json"
	"log"
	"net/http"
	"sync/atomic"

	"github.com/gorilla/websocket"
)

// AudioConfig is the JSON sent as the first WebSocket text message
type AudioConfig struct {
	Type       string `json:"type"`       // "audio.config"
	Encoding   string `json:"encoding"`   // e.g. "webm_opus", "linear16", "mulaw"
	SampleRate int    `json:"sample_rate"`
	Channels   int    `json:"channels"`
	Language   string `json:"language,omitempty"` // BCP-47
	Source     string `json:"source,omitempty"`   // "browser", "twilio", "vonage", "mobile"
}

// AudioControl is a JSON control message (e.g. audio.end)
type AudioControl struct {
	Type string `json:"type"` // "audio.end"
}

// AudioHandler is called for each audio segment. config is the session config,
// data is the accumulated audio bytes for this segment. Called in a goroutine.
type AudioHandler func(conversationID string, config AudioConfig, data []byte)

var wsUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true // CORS handled by middleware
	},
}

// HandleAudioStream handles WS /api/conversations/{id}/audio
//
// Protocol:
//  1. Client sends JSON text frame: AudioConfig (type: "audio.config")
//  2. Client sends binary frames: raw audio bytes
//  3. Client sends JSON text frame: AudioControl (type: "audio.end") to end a segment
//  4. Server processes segment and sends SSE-style JSON responses as text frames
//  5. Client can repeat 1-3 for multiple segments on the same connection
func (h *Handlers) HandleAudioStream(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Validate session
	session, err := h.sessionManager.ValidateRequest(ctx, r)
	if err != nil {
		http.Error(w, "Authentication error", http.StatusInternalServerError)
		return
	}
	if session == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	conversationID := r.PathValue("id")
	if conversationID == "" {
		http.Error(w, "Missing conversation ID", http.StatusBadRequest)
		return
	}

	// Upgrade to WebSocket
	ws, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[Web] WebSocket upgrade failed: %v", err)
		return
	}
	defer ws.Close()

	log.Printf("[Web] Audio WebSocket opened: conversation=%s, user=%s", conversationID, session.UserID)

	// Register an SSE connection so agent responses get routed back to this WebSocket
	conn := &SSEConnection{
		ID:             "ws-audio-" + conversationID,
		ConversationID: conversationID,
		EventChan:      make(chan SSEEvent, 100),
		Done:           make(chan struct{}),
	}
	h.connManager.Add(conn)
	defer h.connManager.Remove(conversationID, conn.ID)

	// Forward SSE events as WebSocket text frames
	var wsClosed atomic.Bool
	go func() {
		for {
			select {
			case <-conn.Done:
				return
			case event, ok := <-conn.EventChan:
				if !ok {
					return
				}
				if wsClosed.Load() {
					return
				}
				// Send as JSON matching SSE format: {"event": "...", "data": ...}
				envelope := map[string]any{
					"event": event.Event,
					"data":  json.RawMessage(event.Data),
				}
				msg, _ := json.Marshal(envelope)
				if err := ws.WriteMessage(websocket.TextMessage, msg); err != nil {
					log.Printf("[Web] Audio WS write error: %v", err)
					return
				}
			}
		}
	}()

	// Read loop: config → binary chunks → audio.end, repeat
	var currentConfig *AudioConfig
	var audioBuffer []byte

	for {
		msgType, data, err := ws.ReadMessage()
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				log.Printf("[Web] Audio WebSocket closed normally: conversation=%s", conversationID)
			} else {
				log.Printf("[Web] Audio WebSocket read error: %v", err)
			}
			break
		}

		switch msgType {
		case websocket.TextMessage:
			// JSON control message
			var ctrl AudioControl
			if err := json.Unmarshal(data, &ctrl); err != nil {
				log.Printf("[Web] Audio WS invalid JSON: %v", err)
				continue
			}

			switch ctrl.Type {
			case "audio.config":
				var config AudioConfig
				if err := json.Unmarshal(data, &config); err != nil {
					log.Printf("[Web] Audio WS invalid config: %v", err)
					continue
				}
				currentConfig = &config
				audioBuffer = audioBuffer[:0]
				log.Printf("[Web] Audio config: encoding=%s, sampleRate=%d, source=%s",
					config.Encoding, config.SampleRate, config.Source)

			case "audio.end":
				if currentConfig == nil {
					log.Printf("[Web] Audio WS got audio.end without config")
					continue
				}
				if len(audioBuffer) == 0 {
					log.Printf("[Web] Audio WS got audio.end with no data")
					continue
				}

				// Forward audio to message handler as an attachment-bearing message
				h.handleAudioSegment(ctx, conversationID, session, currentConfig, audioBuffer)
				audioBuffer = audioBuffer[:0]

			default:
				log.Printf("[Web] Audio WS unknown control type: %s", ctrl.Type)
			}

		case websocket.BinaryMessage:
			// Raw audio bytes
			audioBuffer = append(audioBuffer, data...)
		}
	}

	wsClosed.Store(true)

	// If there's buffered audio when the connection closes, process it
	if currentConfig != nil && len(audioBuffer) > 0 {
		h.handleAudioSegment(ctx, conversationID, session, currentConfig, audioBuffer)
	}

	log.Printf("[Web] Audio WebSocket handler done: conversation=%s", conversationID)
}
