// This file contains the audio message construction and batch upload handler.
//
// When audio arrives (via WebSocket or file upload), we need to:
//   1. Create a Message proto with "[audio]" content and audio metadata (encoding, etc.)
//   2. Forward it to the agent via the message handler (same path as text messages)
//   3. Stream the actual audio bytes via the AudioForwarder (gRPC)
//
// The "[audio]" message serves as a placeholder that the agent can later update
// with the actual transcript text once STT completes.
//
// This file also contains encoding conversion utilities (string ↔ proto ↔ MIME).

package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// encodingToMIME maps the short encoding names used in AudioConfig.Encoding
// to standard MIME types. Used when constructing the Attachment proto so the
// agent knows what content-type the audio data is.
var encodingToMIME = map[string]string{
	"linear16":  "audio/L16",
	"mulaw":     "audio/basic",
	"opus":      "audio/opus",
	"mp3":       "audio/mpeg",
	"webm_opus": "audio/webm;codecs=opus",
	"ogg_opus":  "audio/ogg;codecs=opus",
	"flac":      "audio/flac",
	"aac":       "audio/aac",
}

// handleAudioSegmentStart creates and sends a placeholder "[audio]" message to the agent.
//
// This is called when a new audio segment begins (on audio.config for WebSocket,
// or before sending audio data for uploads). The message carries:
//   - User identity (who is speaking)
//   - Audio metadata in PlatformData (encoding, sample rate, channels, language, source)
//   - An AUDIO attachment with the correct MIME type
//
// The agent receives this as a normal incoming message. It can use the platform_data
// to configure its STT provider, and later send a Transcript response to replace
// the "[audio]" placeholder with the actual transcribed text.
//
// Returns the generated message ID so callers can include it in HTTP responses.
func (h *Handlers) handleAudioSegmentStart(
	ctx context.Context,
	conversationID string,
	session *Session,
	config *AudioConfig,
) string {
	if h.msgHandler == nil {
		slog.Info("[Web] No message handler registered, dropping audio segment start")
		return ""
	}

	messageID := uuid.NewString()
	now := time.Now()

	mimeType := encodingToMIME[config.Encoding]
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}

	// Build a Message proto that looks like a regular user message but with
	// "[audio]" as content and audio metadata attached. This flows through
	// the same HandleIncomingMessage path as text messages.
	msg := &pb.Message{
		Id:        messageID,
		Timestamp: timestamppb.New(now),
		Platform:  "web",
		PlatformContext: &pb.PlatformContext{
			MessageId: messageID,
			ChannelId: conversationID,
			// Audio metadata stored in platform_data so the agent can read it
			// without needing to understand the Attachment proto
			PlatformData: map[string]string{
				"audio_encoding":    config.Encoding,
				"audio_sample_rate": fmt.Sprintf("%d", config.SampleRate),
				"audio_channels":    fmt.Sprintf("%d", config.Channels),
				"audio_language":    config.Language,
				"audio_source":      config.Source,
			},
		},
		User: &pb.User{
			Id:       session.UserID,
			Username: session.Username,
			Email:    session.Email,
		},
		Content:        "[audio]", // Placeholder — agent can update via Transcript response
		ConversationId: conversationID,
		Attachments: []*pb.Attachment{
			{
				Type:     pb.Attachment_AUDIO,
				Filename: fmt.Sprintf("audio.%s", encodingToExtension(config.Encoding)),
				MimeType: mimeType,
			},
		},
	}

	if err := h.msgHandler(ctx, msg); err != nil {
		slog.Error(fmt.Sprintf("[Web] Error forwarding audio message: %v", err))
		h.sendErrorEvent(conversationID, "INTERNAL_ERROR", "Failed to process audio")
	}

	return messageID
}

// audioConfigToProto converts a WebSocket-layer AudioConfig (JSON-based) to a
// protobuf AudioStreamConfig that can be sent over the gRPC stream to the agent.
func audioConfigToProto(config *AudioConfig, conversationID string, userID string) *pb.AudioStreamConfig {
	return &pb.AudioStreamConfig{
		Encoding:       encodingToProto(config.Encoding),
		SampleRate:     int32(config.SampleRate), //nolint:gosec
		Channels:       int32(config.Channels),   //nolint:gosec
		Language:       config.Language,
		ConversationId: conversationID,
		Source:         config.Source,
		UserId:         userID,
	}
}

// HandleAudioUpload handles POST /api/conversations/{id}/audio.
//
// This is the batch (non-streaming) audio upload endpoint for use cases where
// the client has a complete audio file rather than a live microphone stream:
//   - Pre-recorded voice messages
//   - File uploads (drag-and-drop audio files)
//   - Alexa-style single-request voice interactions
//
// The request is a multipart form with:
//   - "audio" file field: the audio data
//   - "encoding" (optional): override encoding detection (e.g. "webm_opus")
//   - "language" (optional): BCP-47 language hint (e.g. "en-US")
//   - "source" (optional): origin identifier (defaults to "upload")
//
// The handler sends the audio through the same pipeline as WebSocket audio:
// AudioStreamConfig → "[audio]" message → AudioChunk(done=true). The only
// difference is all audio arrives in one shot rather than being streamed.
func (h *Handlers) HandleAudioUpload(w http.ResponseWriter, r *http.Request) {
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

	// Cap request body at 25MB to prevent memory exhaustion from large uploads
	r.Body = http.MaxBytesReader(w, r.Body, 25<<20)
	if err := r.ParseMultipartForm(25 << 20); err != nil { //nolint:gosec // G120: body is already capped via MaxBytesReader on the line above
		http.Error(w, "Invalid multipart form", http.StatusBadRequest)
		return
	}

	file, header, err := r.FormFile("audio")
	if err != nil {
		http.Error(w, "Missing 'audio' file field", http.StatusBadRequest)
		return
	}
	defer func() { _ = file.Close() }()

	audioData, err := io.ReadAll(file)
	if err != nil {
		http.Error(w, "Failed to read audio file", http.StatusInternalServerError)
		return
	}

	// Build config from explicit form fields, falling back to content-type detection
	config := AudioConfig{
		Encoding:   r.FormValue("encoding"),
		SampleRate: 16000, // Default to 16kHz — common for speech STT models
		Channels:   1,     // Mono — standard for voice
		Language:   r.FormValue("language"),
		Source:     r.FormValue("source"),
	}

	if config.Encoding == "" {
		// Infer encoding from the file's Content-Type header in the multipart form
		config.Encoding = mimeToEncoding(header.Header.Get("Content-Type"))
	}
	if config.Source == "" {
		config.Source = "upload"
	}

	// Send through the same pipeline as WebSocket audio, in the correct order:
	//   1. AudioStreamConfig → tells agent the format
	//   2. "[audio]" message → gives agent user context
	//   3. AudioChunk(done=true) → the complete audio data in one chunk
	if h.audioForwarder != nil {
		protoConfig := audioConfigToProto(&config, conversationID, session.UserID)
		if err := h.audioForwarder.SendAudioConfig(conversationID, protoConfig); err != nil {
			slog.Error(fmt.Sprintf("[Web] Error sending upload audio config: %v", err))
		}
	}
	messageID := h.handleAudioSegmentStart(r.Context(), conversationID, session, &config)
	if h.audioForwarder != nil {
		// Send as a single chunk with done=true since all data is available at once
		if err := h.audioForwarder.SendAudioChunk(conversationID, audioData, 1, true); err != nil {
			slog.Error(fmt.Sprintf("[Web] Error sending upload audio data: %v", err))
		}
	}

	// Return the same message ID that the agent received, so the client can
	// correlate the upload with the agent's transcript response
	resp := map[string]string{
		"message_id": messageID,
		"status":     "accepted",
		"size_bytes": fmt.Sprintf("%d", len(audioData)),
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		slog.Error(fmt.Sprintf("[Web] Encode error on audio upload response: %v", err))
	}

	slog.Info(fmt.Sprintf("[Web] Audio upload accepted: conversation=%q, file=%q, size=%d", //nolint:gosec // G706 false positive: %q escapes control characters
		conversationID, header.Filename, len(audioData)))
}

// --- Encoding conversion utilities ---
//
// Audio can arrive in many formats depending on the source:
//   - Browser MediaRecorder → webm_opus (default) or ogg_opus
//   - Twilio Media Streams  → mulaw (G.711 mu-law, 8kHz)
//   - Mobile apps           → aac (iOS) or opus (Android)
//   - File uploads          → mp3, flac, wav, etc.
//
// These utilities convert between three representations:
//   - String names ("webm_opus") — used in WebSocket JSON and config
//   - Proto enums (AudioEncoding_WEBM_OPUS) — used on the gRPC wire
//   - MIME types ("audio/webm;codecs=opus") — used in HTTP headers and Attachments

// encodingToExtension maps encoding names to file extensions for the Attachment filename.
func encodingToExtension(encoding string) string {
	switch encoding {
	case "webm_opus":
		return "webm"
	case "ogg_opus":
		return "ogg"
	case "linear16":
		return "wav"
	case "mulaw":
		return "wav"
	case "opus":
		return "opus"
	case "mp3":
		return "mp3"
	case "flac":
		return "flac"
	case "aac":
		return "m4a"
	default:
		return "bin"
	}
}

// encodingToProto converts a string encoding name to the protobuf AudioEncoding enum.
func encodingToProto(encoding string) pb.AudioEncoding {
	switch strings.ToLower(encoding) {
	case "linear16":
		return pb.AudioEncoding_LINEAR16
	case "mulaw":
		return pb.AudioEncoding_MULAW
	case "opus":
		return pb.AudioEncoding_OPUS
	case "mp3":
		return pb.AudioEncoding_MP3
	case "webm_opus":
		return pb.AudioEncoding_WEBM_OPUS
	case "ogg_opus":
		return pb.AudioEncoding_OGG_OPUS
	case "flac":
		return pb.AudioEncoding_FLAC
	case "aac":
		return pb.AudioEncoding_AAC
	default:
		return pb.AudioEncoding_AUDIO_ENCODING_UNSPECIFIED
	}
}

// mimeToEncoding infers the encoding name from a MIME type. Used by the upload
// handler when the client doesn't explicitly set the "encoding" form field.
// Defaults to "webm_opus" as that's the most common browser recording format.
func mimeToEncoding(mime string) string {
	switch mime {
	case "audio/webm", "audio/webm;codecs=opus":
		return "webm_opus"
	case "audio/ogg", "audio/ogg;codecs=opus":
		return "ogg_opus"
	case "audio/wav", "audio/x-wav", "audio/L16":
		return "linear16"
	case "audio/mpeg", "audio/mp3":
		return "mp3"
	case "audio/flac":
		return "flac"
	case "audio/aac", "audio/mp4":
		return "aac"
	case "audio/opus":
		return "opus"
	default:
		return "webm_opus" // sensible default for browser uploads
	}
}
