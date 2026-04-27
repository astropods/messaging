// Package web provides the HTTP/WebSocket adapter for browser-based clients.
//
// This file implements the WebSocket audio ingestion endpoint. It handles real-time
// audio streaming from browser clients (via MediaRecorder API) and other WebSocket
// sources into the messaging system.
//
// Architecture overview:
//
//	Browser (mic) ──WebSocket──> audio.go ──gRPC──> Agent (Mastra voice.listen)
//	                                │
//	                                └──> audio_handler.go (message metadata)
//
// The WebSocket is ingest-only: audio flows client→server. Agent responses flow
// back to the client through the existing SSE (Server-Sent Events) stream on a
// separate connection. This separation keeps the audio path simple and avoids
// multiplexing concerns.
//
// Protocol (one WebSocket connection = one conversation session):
//
//	1. Client sends JSON text frame:  { "type": "audio.config", "encoding": "webm_opus", ... }
//	2. Client sends binary frames:    raw audio bytes (from MediaRecorder.ondataavailable)
//	3. Client sends JSON text frame:  { "type": "audio.end" }
//	4. Repeat 1-3 for additional utterances on the same connection
//
// Each audio.config → binary chunks → audio.end cycle is called a "segment".
// A segment represents one utterance (push-to-talk release, VAD silence, etc.).
// The client decides when to segment — the server does no voice activity detection.
package web

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/gorilla/websocket"
)

// AudioConfig is the JSON payload the client sends as the first text frame
// to configure an audio segment. It tells the agent what format the audio
// bytes will be in so it can pass them to the correct STT provider.
//
// Example from a browser using MediaRecorder:
//
//	{ "type": "audio.config", "encoding": "webm_opus", "sample_rate": 48000,
//	  "channels": 1, "language": "en-US", "source": "browser" }
type AudioConfig struct {
	Type       string `json:"type"`                  // Must be "audio.config"
	Encoding   string `json:"encoding"`              // Audio codec: "webm_opus", "linear16", "mulaw", etc.
	SampleRate int    `json:"sample_rate"`            // Sample rate in Hz (e.g. 48000 for browser, 8000 for Twilio)
	Channels   int    `json:"channels"`               // 1 = mono (typical for speech), 2 = stereo
	Language   string `json:"language,omitempty"`      // BCP-47 language hint for STT (e.g. "en-US")
	Source     string `json:"source,omitempty"`        // Origin identifier: "browser", "twilio", "mobile", etc.
}

// AudioControl is a JSON control message sent as a text frame to signal
// end-of-segment. The only supported type is "audio.end".
type AudioControl struct {
	Type string `json:"type"` // "audio.end"
}

// AudioHandler is a callback signature for processing completed audio segments.
// Currently unused (audio is streamed chunk-by-chunk via AudioForwarder), but
// retained for potential batch-processing use cases.
type AudioHandler func(conversationID string, config AudioConfig, data []byte)

// maxAudioMessageSize limits individual WebSocket frames to 1MB.
// This prevents a client from sending a single massive binary frame that
// exhausts server memory. Typical audio chunks from MediaRecorder are
// ~10-50KB, so 1MB provides ample headroom while capping memory usage.
const maxAudioMessageSize = 1 * 1024 * 1024

// wsUpgrader is the package-level WebSocket upgrader shared by all audio connections.
// CheckOrigin returns true to allow connections from any origin — this is intentional
// as the messaging system is designed to be embeddable in any frontend domain.
// Authentication is handled at the session level, not via origin restrictions.
var wsUpgrader = websocket.Upgrader{
	ReadBufferSize:  64 * 1024, // 64KB — matches typical audio chunk sizes
	WriteBufferSize: 4 * 1024,  // 4KB — this is an ingest-only socket, minimal server writes
	CheckOrigin: func(r *http.Request) bool {
		return true // intentionally open; auth is via session tokens, not origin
	},
}

// HandleAudioStream handles WebSocket connections at GET /api/conversations/{id}/audio.
//
// This is the main audio ingestion entry point. When a client wants to send voice
// input, it opens a WebSocket here and streams audio bytes. The handler forwards
// each chunk to the agent in real time via the AudioForwarder (gRPC stream).
//
// The lifecycle of a typical voice interaction:
//
//  1. Client opens WebSocket connection
//  2. Client sends audio.config (JSON) → handler sends AudioStreamConfig to agent via gRPC,
//     then sends a placeholder "[audio]" message to the agent for context
//  3. Client sends binary audio frames → handler forwards each as an AudioChunk to the agent
//  4. Client sends audio.end (JSON) → handler sends a final AudioChunk with done=true
//  5. Agent processes the audio (STT via Mastra), generates a text response
//  6. Agent response arrives back through the existing SSE stream (not this WebSocket)
//
// If the WebSocket closes unexpectedly mid-segment (e.g. network drop), the handler
// automatically sends a done=true chunk to flush whatever audio the agent has received.
func (h *Handlers) HandleAudioStream(w http.ResponseWriter, r *http.Request) {
	// Authenticate the request (session) and authorize against this
	// deployment's grants. authenticate writes the response on failure.
	session := h.authenticate(w, r)
	if session == nil {
		return
	}

	conversationID := r.PathValue("id")
	if conversationID == "" {
		http.Error(w, "Missing conversation ID", http.StatusBadRequest)
		return
	}

	// Upgrade HTTP connection to WebSocket
	ws, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Error(fmt.Sprintf("[Web] WebSocket upgrade failed: %v", err))
		return
	}
	defer func() { _ = ws.Close() }()

	// Cap individual frame size to prevent memory exhaustion
	ws.SetReadLimit(maxAudioMessageSize)

	slog.Info(fmt.Sprintf("[Web] Audio WebSocket opened: conversation=%q, user=%q", conversationID, session.UserID)) //nolint:gosec // G706 false positive: %q escapes control characters, preventing log injection

	// --- Read loop: stream audio through to the agent in real time ---
	//
	// State machine per connection:
	//   IDLE → (audio.config) → ACTIVE → (binary chunks) → ACTIVE → (audio.end) → IDLE
	//
	// Multiple segments can be sent on the same connection (e.g. a voice assistant
	// where the user speaks, gets a response, then speaks again).

	var currentConfig *AudioConfig
	var chunkCount int    // number of binary frames received in this segment
	var totalBytes int    // total audio bytes received in this segment
	var segmentActive bool // true between audio.config and audio.end

	for {
		msgType, data, err := ws.ReadMessage()
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				slog.Info(fmt.Sprintf("[Web] Audio WebSocket closed normally: conversation=%q", conversationID)) //nolint:gosec // G706 false positive: %q escapes control characters
			} else {
				slog.Error(fmt.Sprintf("[Web] Audio WebSocket read error: %v", err))
			}
			break
		}

		switch msgType {
		case websocket.TextMessage:
			// Text frames are JSON control messages (audio.config or audio.end)
			var ctrl AudioControl
			if err := json.Unmarshal(data, &ctrl); err != nil {
				slog.Error(fmt.Sprintf("[Web] Audio WS invalid JSON: %v", err))
				continue
			}

			switch ctrl.Type {
			case "audio.config":
				// Parse the full config (AudioControl only has "type"; AudioConfig has all fields)
				var config AudioConfig
				if err := json.Unmarshal(data, &config); err != nil {
					slog.Error(fmt.Sprintf("[Web] Audio WS invalid config: %v", err))
					continue
				}
				currentConfig = &config
				chunkCount = 0
				totalBytes = 0
				slog.Info(fmt.Sprintf("[Web] Audio config: encoding=%s, sampleRate=%d, source=%s",
					config.Encoding, config.SampleRate, config.Source))

				// Order matters here: send the audio format config to the agent FIRST,
				// then the message metadata. This ensures the agent knows the encoding
				// before it receives the "[audio]" message that may trigger it to start
				// listening for audio chunks.
				if h.audioForwarder != nil {
					protoConfig := audioConfigToProto(&config, conversationID, session.UserID)
					if err := h.audioForwarder.SendAudioConfig(conversationID, protoConfig); err != nil {
						slog.Error(fmt.Sprintf("[Web] Error sending audio config to agent: %v", err))
					}
				}

				// Send a placeholder "[audio]" message to the agent with metadata
				// (user info, encoding details, etc.) so it has full context
				h.handleAudioSegmentStart(r.Context(), conversationID, session, &config)
				segmentActive = true

			case "audio.end":
				// Client signals end of this utterance. Could be push-to-talk release,
				// client-side VAD silence detection, or explicit user action.
				if !segmentActive {
					slog.Info("[Web] Audio WS got audio.end without active segment")
					continue
				}

				// Tell the agent this segment is complete by sending a zero-length
				// chunk with done=true. The agent can now run STT on the accumulated audio.
				if h.audioForwarder != nil {
					if err := h.audioForwarder.SendAudioChunk(conversationID, nil, int64(chunkCount+1), true); err != nil {
						slog.Error(fmt.Sprintf("[Web] Error sending audio end to agent: %v", err))
					}
				}

				slog.Info(fmt.Sprintf("[Web] Audio segment complete: conversation=%q, chunks=%d, total=%d bytes, encoding=%q", //nolint:gosec // G706 false positive: %q escapes control characters
					conversationID, chunkCount, totalBytes, currentConfig.Encoding))
				segmentActive = false

			default:
				slog.Info(fmt.Sprintf("[Web] Audio WS unknown control type: %s", ctrl.Type))
			}

		case websocket.BinaryMessage:
			// Binary frames contain raw audio bytes from the client's microphone.
			// These are forwarded to the agent as-is — no transcoding, no buffering.
			if !segmentActive {
				slog.Info("[Web] Audio WS got binary data without active segment, dropping")
				continue
			}
			chunkCount++
			totalBytes += len(data)
			if h.audioForwarder != nil {
				if err := h.audioForwarder.SendAudioChunk(conversationID, data, int64(chunkCount), false); err != nil {
					slog.Error(fmt.Sprintf("[Web] Error streaming audio chunk to agent: %v", err))
				}
			}
			// Log progress every 20 chunks to avoid flooding logs
			// (at ~50ms per chunk from MediaRecorder, this logs roughly once per second)
			if chunkCount%20 == 1 {
				slog.Info(fmt.Sprintf("[Web] Audio WS streaming: conversation=%q, chunks=%d, total=%d bytes", //nolint:gosec // G706 false positive: %q escapes control characters
					conversationID, chunkCount, totalBytes))
			}
		}
	}

	// If the WebSocket closed mid-segment (e.g. network drop, tab close),
	// flush whatever audio the agent has by sending done=true. This ensures
	// the agent doesn't hang waiting for more audio that will never arrive.
	if segmentActive && h.audioForwarder != nil {
		_ = h.audioForwarder.SendAudioChunk(conversationID, nil, int64(chunkCount+1), true)
		slog.Info(fmt.Sprintf("[Web] Audio segment flushed on close: conversation=%q, chunks=%d, total=%d bytes", //nolint:gosec // G706 false positive: %q escapes control characters
			conversationID, chunkCount, totalBytes))
	}

	slog.Info(fmt.Sprintf("[Web] Audio WebSocket handler done: conversation=%q", conversationID)) //nolint:gosec // G706 false positive: %q escapes control characters
}
