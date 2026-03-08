package adapter

import (
	"context"

	"github.com/astropods/messaging/internal/store"
	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
)

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

// AudioForwarder streams audio data to the agent via the gRPC stream.
type AudioForwarder interface {
	SendAudioConfig(conversationID string, config *pb.AudioStreamConfig) error
	SendAudioChunk(conversationID string, data []byte, sequence int64, done bool) error
}

// Config holds adapter configuration
type Config struct {
	BotToken   string
	AppToken   string // For Slack Socket Mode
	SocketMode bool
	WebhookURL string
	AutoThread bool
	RateLimit  RateLimitConfig
}

// RateLimitConfig configures rate limiting
type RateLimitConfig struct {
	RequestsPerSecond float64
	BurstSize         int
}
