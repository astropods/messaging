// Package telephony provides WebSocket handlers for telephony providers
// (Twilio, Vonage, etc.) that normalize their audio protocols into the
// messaging system's AudioConfig + audio bytes pipeline.
package telephony

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/google/uuid"
)

// AudioSegmentHandler is called when a complete audio segment is ready.
// conversationID ties it to a conversation, config describes the audio format,
// and data contains the raw audio bytes.
type AudioSegmentHandler func(conversationID string, config AudioConfig, data []byte)

// AudioConfig mirrors the web adapter's config for consistency
type AudioConfig struct {
	Encoding   string
	SampleRate int
	Channels   int
	Language   string
	Source     string
}

// --- Twilio Media Streams ---
// See: https://www.twilio.com/docs/voice/media-streams/websocket-messages

// twilioMessage is the envelope for all Twilio WebSocket messages
type twilioMessage struct {
	Event     string          `json:"event"`
	StreamSid string         `json:"streamSid"`
	Start     *twilioStart   `json:"start,omitempty"`
	Media     *twilioMedia   `json:"media,omitempty"`
	Mark      *twilioMark    `json:"mark,omitempty"`
}

type twilioStart struct {
	StreamSid   string            `json:"streamSid"`
	AccountSid  string            `json:"accountSid"`
	CallSid     string            `json:"callSid"`
	MediaFormat twilioMediaFormat `json:"mediaFormat"`
	CustomParameters map[string]string `json:"customParameters"`
}

type twilioMediaFormat struct {
	Encoding   string `json:"encoding"`   // "audio/x-mulaw"
	SampleRate int    `json:"sampleRate"` // 8000
	Channels   int    `json:"channels"`   // 1
}

type twilioMedia struct {
	Track     string `json:"track"`     // "inbound" or "outbound"
	Chunk     string `json:"chunk"`
	Timestamp string `json:"timestamp"`
	Payload   string `json:"payload"`   // base64-encoded audio
}

type twilioMark struct {
	Name string `json:"name"`
}

var twUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true // Twilio connects to us; origin doesn't apply
	},
}

// TwilioHandler handles Twilio Media Streams WebSocket connections.
type TwilioHandler struct {
	onSegment AudioSegmentHandler
}

// NewTwilioHandler creates a handler that calls onSegment with decoded audio.
func NewTwilioHandler(onSegment AudioSegmentHandler) *TwilioHandler {
	return &TwilioHandler{onSegment: onSegment}
}

// ServeHTTP handles WS /api/telephony/twilio/stream
func (h *TwilioHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ws, err := twUpgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[Twilio] WebSocket upgrade failed: %v", err)
		return
	}
	defer ws.Close()

	log.Printf("[Twilio] WebSocket connected from %s", r.RemoteAddr)

	var (
		config         *AudioConfig
		audioBuffer    []byte
		streamSid      string
		callSid        string
		conversationID string
		closed         atomic.Bool
	)

	// Set read deadline for idle timeout (5 minutes)
	ws.SetReadDeadline(time.Now().Add(5 * time.Minute))
	ws.SetPongHandler(func(string) error {
		ws.SetReadDeadline(time.Now().Add(5 * time.Minute))
		return nil
	})

	for {
		_, data, err := ws.ReadMessage()
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				log.Printf("[Twilio] WebSocket closed normally: stream=%s", streamSid)
			} else if !closed.Load() {
				log.Printf("[Twilio] WebSocket read error: %v", err)
			}
			break
		}

		// Reset read deadline on activity
		ws.SetReadDeadline(time.Now().Add(5 * time.Minute))

		var msg twilioMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			log.Printf("[Twilio] Invalid JSON: %v", err)
			continue
		}

		switch msg.Event {
		case "connected":
			log.Printf("[Twilio] Stream connected")

		case "start":
			if msg.Start == nil {
				continue
			}
			streamSid = msg.Start.StreamSid
			callSid = msg.Start.CallSid
			conversationID = fmt.Sprintf("twilio-%s", callSid)
			if conversationID == "twilio-" {
				conversationID = fmt.Sprintf("twilio-%s", uuid.NewString())
			}

			config = &AudioConfig{
				Encoding:   "mulaw",
				SampleRate: msg.Start.MediaFormat.SampleRate,
				Channels:   msg.Start.MediaFormat.Channels,
				Source:     "twilio",
			}
			if config.SampleRate == 0 {
				config.SampleRate = 8000
			}
			if config.Channels == 0 {
				config.Channels = 1
			}

			// Check for language in custom parameters
			if lang, ok := msg.Start.CustomParameters["language"]; ok {
				config.Language = lang
			}

			audioBuffer = audioBuffer[:0]
			log.Printf("[Twilio] Stream started: streamSid=%s, callSid=%s, conversation=%s",
				streamSid, callSid, conversationID)

		case "media":
			if msg.Media == nil || config == nil {
				continue
			}
			// Only process inbound audio (caller's voice)
			if msg.Media.Track != "inbound" {
				continue
			}

			decoded, err := base64.StdEncoding.DecodeString(msg.Media.Payload)
			if err != nil {
				log.Printf("[Twilio] base64 decode error: %v", err)
				continue
			}
			audioBuffer = append(audioBuffer, decoded...)

		case "stop":
			closed.Store(true)
			if config != nil && len(audioBuffer) > 0 && h.onSegment != nil {
				log.Printf("[Twilio] Stream stopped, forwarding %d bytes: conversation=%s",
					len(audioBuffer), conversationID)
				h.onSegment(conversationID, *config, audioBuffer)
			}
			audioBuffer = nil
			log.Printf("[Twilio] Stream stopped: streamSid=%s", streamSid)

		case "mark":
			// Mark events are acknowledgments; no action needed
			log.Printf("[Twilio] Mark received: %s", msg.Mark.Name)

		default:
			log.Printf("[Twilio] Unknown event: %s", msg.Event)
		}
	}

	// If connection closed without a stop event, process remaining audio
	if !closed.Load() && config != nil && len(audioBuffer) > 0 && h.onSegment != nil {
		log.Printf("[Twilio] Connection closed with buffered audio, forwarding %d bytes", len(audioBuffer))
		h.onSegment(conversationID, *config, audioBuffer)
	}
}
