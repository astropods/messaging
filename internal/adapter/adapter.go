package adapter

import (
	"context"
	"errors"

	"github.com/astropods/messaging/internal/store"
	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
)

// ErrNoAgentStream is returned when a message cannot be delivered because
// no agent has connected to the gRPC stream. Adapters should log this
// rather than surfacing it to end users.
var ErrNoAgentStream = errors.New("no active agent stream available")

// Adapter is the interface that all platform adapters must implement
type Adapter interface {
	// Lifecycle
	Initialize(ctx context.Context, config Config) error
	Start(ctx context.Context) error
	Stop(ctx context.Context) error

	// Identity & health
	GetPlatformName() string
	IsHealthy(ctx context.Context) bool
	Capabilities() AdapterCapabilities

	// Message handling (platform → agent)
	SetMessageHandler(handler MessageHandler)

	// Response handling (agent → platform)
	HandleAgentResponse(ctx context.Context, response *pb.AgentResponse) error

	// Thread history
	HydrateThread(ctx context.Context, conversationID string, store *store.ThreadHistoryStore) error
}

// MessageHandler is called when a message is received from the platform.
// It should forward the message to the gRPC server which sends it to the agent.
type MessageHandler func(ctx context.Context, msg *pb.Message) error

// AudioForwarder streams audio data to the agent via the gRPC bidirectional stream.
//
// This interface decouples the WebSocket audio ingestion (web adapter) from the
// gRPC transport layer. The web adapter calls these methods as audio arrives;
// the gRPC server implements them by wrapping the data in AgentResponse protos
// and sending them on the active agent stream.
//
// Data flow:
//
//	WebSocket (audio.go) → AudioForwarder → gRPC stream → Agent SDK (Node)
//
// The agent SDK receives these as 'audioConfig' and 'audioChunk' events on
// the ConversationStream, and can pipe them to Mastra's voice.listen().
type AudioForwarder interface {
	// SendAudioConfig sends the audio format configuration at the start of a segment.
	// Must be called before SendAudioChunk so the agent knows how to decode the bytes.
	SendAudioConfig(conversationID string, config *pb.AudioStreamConfig) error

	// SendAudioChunk sends a chunk of raw audio bytes to the agent.
	// sequence is a monotonically increasing counter for ordering.
	// done=true signals end of the current audio segment (agent should run STT).
	SendAudioChunk(conversationID string, data []byte, sequence int64, done bool) error
}

// Config holds adapter configuration
type Config struct {
	BotToken            string
	AppToken            string // For Slack Socket Mode
	SocketMode          bool
	WebhookURL          string
	AutoThread          bool
	DevMode             bool // When true, messages include a "sent from dev" context
	// AgentID is the value of ASTRO_AGENT_ID at startup. When non-empty it is
	// rendered in the Slack message footer so users know which agent replied.
	AgentID             string
	RateLimit           RateLimitConfig
	ActionableReactions []string // Emoji names forwarded to the agent; empty means no reactions
	AllowedChannelIDs   []string // Channel IDs that may use the app (empty = allow all)
	AllowedUserIDs      []string // User IDs that may use the app (empty = allow all)

	// ObserveChannelIDs lists channel IDs where top-level (non-mention) messages
	// are forwarded to the agent instead of being dropped. A message that already
	// mentions the bot user is delivered via app_mention only (not duplicated).
	ObserveChannelIDs []string
}

// RateLimitConfig configures rate limiting
type RateLimitConfig struct {
	RequestsPerSecond float64
	BurstSize         int
}
