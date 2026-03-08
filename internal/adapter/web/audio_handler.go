package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// encodingToMIME maps AudioConfig.Encoding to MIME types
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

// handleAudioSegmentStart sends the audio message metadata to the agent.
// Called on audio.config before any audio chunks are streamed.
func (h *Handlers) handleAudioSegmentStart(
	ctx context.Context,
	conversationID string,
	session *Session,
	config *AudioConfig,
) {
	if h.msgHandler == nil {
		log.Printf("[Web] No message handler registered, dropping audio segment start")
		return
	}

	messageID := uuid.NewString()
	now := time.Now()

	mimeType := encodingToMIME[config.Encoding]
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}

	msg := &pb.Message{
		Id:        messageID,
		Timestamp: timestamppb.New(now),
		Platform:  "web",
		PlatformContext: &pb.PlatformContext{
			MessageId: messageID,
			ChannelId: conversationID,
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
		Content:        "[audio]",
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
		log.Printf("[Web] Error forwarding audio message: %v", err)
		h.sendErrorEvent(conversationID, "INTERNAL_ERROR", "Failed to process audio")
	}
}

// audioConfigToProto converts a WebSocket AudioConfig to a proto AudioStreamConfig.
func audioConfigToProto(config *AudioConfig, conversationID string) *pb.AudioStreamConfig {
	return &pb.AudioStreamConfig{
		Encoding:       encodingToProto(config.Encoding),
		SampleRate:     int32(config.SampleRate), //nolint:gosec
		Channels:       int32(config.Channels),   //nolint:gosec
		Language:       config.Language,
		ConversationId: conversationID,
		Source:         config.Source,
	}
}

// HandleAudioUpload handles POST /api/conversations/{id}/audio
// Accepts multipart form with a single audio file for batch (non-streaming) use cases.
func (h *Handlers) HandleAudioUpload(w http.ResponseWriter, r *http.Request) {
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

	// Parse multipart form (max 25MB)
	if err := r.ParseMultipartForm(25 << 20); err != nil {
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

	// Build config from form fields or content-type
	config := AudioConfig{
		Encoding:   r.FormValue("encoding"),
		SampleRate: 16000,
		Channels:   1,
		Language:   r.FormValue("language"),
		Source:     r.FormValue("source"),
	}

	// Infer encoding from content-type if not provided
	if config.Encoding == "" {
		config.Encoding = mimeToEncoding(header.Header.Get("Content-Type"))
	}
	if config.Source == "" {
		config.Source = "upload"
	}

	// Send message metadata + audio config + full audio as a single chunk
	h.handleAudioSegmentStart(ctx, conversationID, session, &config)
	if h.audioForwarder != nil {
		protoConfig := audioConfigToProto(&config, conversationID)
		if err := h.audioForwarder.SendAudioConfig(conversationID, protoConfig); err != nil {
			log.Printf("[Web] Error sending upload audio config: %v", err)
		}
		if err := h.audioForwarder.SendAudioChunk(conversationID, audioData, 1, true); err != nil {
			log.Printf("[Web] Error sending upload audio data: %v", err)
		}
	}

	resp := map[string]string{
		"message_id": uuid.NewString(),
		"status":     "accepted",
		"size_bytes": fmt.Sprintf("%d", len(audioData)),
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("[Web] Encode error on audio upload response: %v", err)
	}

	log.Printf("[Web] Audio upload accepted: conversation=%s, file=%s, size=%d", //nolint:gosec // conversationID is internal, filename is logged for debugging
		conversationID, header.Filename, len(audioData))
}

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
