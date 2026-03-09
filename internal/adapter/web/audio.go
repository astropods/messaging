package web

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/gorilla/websocket"
)

// AudioConfig is the JSON sent as the first WebSocket text message
type AudioConfig struct {
	Type       string `json:"type"`       // "audio.config"
	Encoding   string `json:"encoding"`   // e.g. "webm_opus", "linear16", "mulaw"
	SampleRate int    `json:"sample_rate"`
	Channels   int    `json:"channels"`
	Language   string `json:"language,omitempty"` // BCP-47
	Source     string `json:"source,omitempty"`   // "browser"
}

// AudioControl is a JSON control message (e.g. audio.end)
type AudioControl struct {
	Type string `json:"type"` // "audio.end"
}

// AudioHandler is called for each audio segment. config is the session config,
// data is the accumulated audio bytes for this segment. Called in a goroutine.
type AudioHandler func(conversationID string, config AudioConfig, data []byte)

// maxAudioMessageSize limits individual WebSocket frames to 1MB to prevent
// memory exhaustion from oversized binary frames.
const maxAudioMessageSize = 1 * 1024 * 1024

// HandleAudioStream handles WS /api/conversations/{id}/audio
//
// This is an ingest-only WebSocket for streaming audio from the client.
// Agent responses flow through the regular conversation SSE stream, not this WebSocket.
//
// Protocol:
//  1. Client sends JSON text frame: AudioConfig (type: "audio.config")
//  2. Client sends binary frames: raw audio bytes
//  3. Client sends JSON text frame: AudioControl (type: "audio.end") to end a segment
//  4. Client can repeat 1-3 for multiple segments on the same connection
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

	// Upgrade to WebSocket with origin validation
	upgrader := websocket.Upgrader{
		ReadBufferSize:  64 * 1024, // 64KB read buffer
		WriteBufferSize: 4 * 1024,  // 4KB write buffer (ingest-only, minimal writes)
		CheckOrigin: func(req *http.Request) bool {
			origin := req.Header.Get("Origin")
			if origin == "" {
				return true // non-browser clients don't send Origin
			}
			if h.originChecker != nil {
				return h.originChecker(origin)
			}
			return true
		},
	}
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[Web] WebSocket upgrade failed: %v", err)
		return
	}
	defer func() { _ = ws.Close() }()

	ws.SetReadLimit(maxAudioMessageSize)

	log.Printf("[Web] Audio WebSocket opened: conversation=%s, user=%s", conversationID, session.UserID)
	// Read loop: stream audio through to the agent in real time
	var currentConfig *AudioConfig
	var chunkCount int
	var totalBytes int
	var segmentActive bool // true after config sent, false after audio.end

	for {
		msgType, data, err := ws.ReadMessage()
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				log.Printf("[Web] Audio WebSocket closed normally: conversation=%s", conversationID)			} else {
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
				chunkCount = 0
				totalBytes = 0
				log.Printf("[Web] Audio config: encoding=%s, sampleRate=%d, source=%s",
					config.Encoding, config.SampleRate, config.Source)

				// Send audio config to agent first so it knows the format
				// before the message metadata triggers processing
				if h.audioForwarder != nil {
					protoConfig := audioConfigToProto(&config, conversationID)
					if err := h.audioForwarder.SendAudioConfig(conversationID, protoConfig); err != nil {
						log.Printf("[Web] Error sending audio config to agent: %v", err)
					}
				}

				// Then send message metadata so the agent has context
				h.handleAudioSegmentStart(ctx, conversationID, session, &config)
				segmentActive = true

			case "audio.end":
				if !segmentActive {
					log.Printf("[Web] Audio WS got audio.end without active segment")
					continue
				}

				// Send final chunk with done=true
				if h.audioForwarder != nil {
					if err := h.audioForwarder.SendAudioChunk(conversationID, nil, int64(chunkCount+1), true); err != nil {
						log.Printf("[Web] Error sending audio end to agent: %v", err)
					}
				}

				log.Printf("[Web] Audio segment complete: conversation=%s, chunks=%d, total=%d bytes, encoding=%s",					conversationID, chunkCount, totalBytes, currentConfig.Encoding)
				segmentActive = false

			default:
				log.Printf("[Web] Audio WS unknown control type: %s", ctrl.Type)
			}

		case websocket.BinaryMessage:
			if !segmentActive {
				log.Printf("[Web] Audio WS got binary data without active segment, dropping")
				continue
			}
			// Stream audio chunk directly to agent
			chunkCount++
			totalBytes += len(data)
			if h.audioForwarder != nil {
				if err := h.audioForwarder.SendAudioChunk(conversationID, data, int64(chunkCount), false); err != nil {
					log.Printf("[Web] Error streaming audio chunk to agent: %v", err)
				}
			}
			if chunkCount%20 == 1 {
				log.Printf("[Web] Audio WS streaming: conversation=%s, chunks=%d, total=%d bytes",					conversationID, chunkCount, totalBytes)
			}
		}
	}

	// If segment was active when connection closed, send done signal
	if segmentActive && h.audioForwarder != nil {
		_ = h.audioForwarder.SendAudioChunk(conversationID, nil, int64(chunkCount+1), true)
		log.Printf("[Web] Audio segment flushed on close: conversation=%s, chunks=%d, total=%d bytes",			conversationID, chunkCount, totalBytes)
	}

	log.Printf("[Web] Audio WebSocket handler done: conversation=%s", conversationID)}
