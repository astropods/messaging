package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/astropods/messaging/internal/adapter"
)

// Config holds the overall messaging service configuration
type Config struct {
	// gRPC Server configuration
	GRPC GRPCConfig

	// Slack configuration
	Slack SlackConfig

	// Web configuration
	Web WebConfig

	// Storage configuration
	Storage StorageConfig

	// Thread history configuration
	ThreadHistory ThreadHistoryConfig

	// Metrics configuration
	Metrics MetricsConfig

	// Authorization (per-deployment grants enforced via callback to astro-server)
	Authz AuthzConfig

	// Logging
	LogLevel string
}

// AuthzConfig holds the configuration the messaging container uses to call
// astro-server's /deployments/authorize endpoint.
//
// Only the token is needed — astro-server's base URL travels inside the
// token's standard `iss` claim, so URL discovery and authentication share
// one source of truth.
type AuthzConfig struct {
	// IdentityToken is the raw HS256 JWT from ASTRO_AUTHZ_TOKEN. Empty
	// disables authz entirely (dev mode — every request allowed).
	IdentityToken string
}

// MetricsConfig holds Prometheus metrics server configuration
type MetricsConfig struct {
	Enabled    bool
	ListenAddr string
}

// GRPCConfig holds gRPC server configuration
type GRPCConfig struct {
	Enabled    bool
	ListenAddr string
	MaxStreams int
}

// ThreadHistoryConfig holds thread history store configuration
type ThreadHistoryConfig struct {
	MaxSize     int // Max number of threads to store
	MaxMessages int // Max messages per thread
	TTL         int // TTL in hours
}

// SlackCredentials holds secret tokens parsed from individual env vars.
type SlackCredentials struct {
	BotToken string
	AppToken string
}

// SlackAdapterConfig holds behavioral settings parsed from the SLACK_CONFIG JSON env var.
type SlackAdapterConfig struct {
	ActionableReactions []string `json:"actionable_reactions,omitempty"`
	SocketMode          *bool    `json:"socket_mode,omitempty"`
	AutoThread          *bool    `json:"auto_thread,omitempty"`
	AllowedChannelIDs   []string `json:"allowed_channel_ids,omitempty"`
	AllowedUserIDs      []string `json:"allowed_user_ids,omitempty"`
}

// SlackConfig holds Slack-specific configuration
type SlackConfig struct {
	Enabled       bool
	Credentials   SlackCredentials
	AdapterConfig SlackAdapterConfig
	Config        adapter.Config
}

// WebConfig holds web adapter configuration
type WebConfig struct {
	Enabled         bool
	ListenAddr      string
	AllowedOrigins  []string
	ServePlayground bool
}

// StorageConfig holds storage configuration
type StorageConfig struct {
	Type     string // "redis" or "memory"
	RedisURL string
	TTL      int // TTL in seconds (default: 7 days)
}

// Load loads configuration from environment variables
func Load() (*Config, error) {
	cfg := &Config{
		LogLevel: getEnv("LOG_LEVEL", "info"),
	}

	// gRPC configuration
	cfg.GRPC = GRPCConfig{
		Enabled:    getEnvBool("GRPC_ENABLED", true),
		ListenAddr: getEnv("GRPC_LISTEN_ADDR", ":9090"),
		MaxStreams: getEnvInt("GRPC_MAX_STREAMS", 100),
	}

	// Thread history configuration
	cfg.ThreadHistory = ThreadHistoryConfig{
		MaxSize:     getEnvInt("THREAD_HISTORY_MAX_SIZE", 1000),
		MaxMessages: getEnvInt("THREAD_HISTORY_MAX_MESSAGES", 50),
		TTL:         getEnvInt("THREAD_HISTORY_TTL_HOURS", 24),
	}

	// Slack configuration: credentials from individual env vars, behavioral
	// settings from SLACK_CONFIG JSON. Defaults match the previous hardcoded values.
	cfg.Slack.Enabled = getEnvBool("SLACK_ENABLED", false)
	cfg.Slack.Credentials = SlackCredentials{
		BotToken: getEnv("SLACK_BOT_TOKEN", ""),
		AppToken: getEnv("SLACK_APP_TOKEN", ""),
	}

	cfg.Slack.AdapterConfig = SlackAdapterConfig{
		SocketMode: boolPtr(true),
		AutoThread: boolPtr(true),
	}
	if raw := os.Getenv("SLACK_CONFIG"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &cfg.Slack.AdapterConfig); err != nil {
			return nil, fmt.Errorf("failed to parse SLACK_CONFIG JSON: %w", err)
		}
		if cfg.Slack.AdapterConfig.SocketMode == nil {
			cfg.Slack.AdapterConfig.SocketMode = boolPtr(true)
		}
		if cfg.Slack.AdapterConfig.AutoThread == nil {
			cfg.Slack.AdapterConfig.AutoThread = boolPtr(true)
		}
	}
	if len(cfg.Slack.AdapterConfig.ActionableReactions) == 0 {
		cfg.Slack.AdapterConfig.ActionableReactions = getEnvList("SLACK_ACTIONABLE_REACTIONS", nil)
	}
	if len(cfg.Slack.AdapterConfig.AllowedChannelIDs) == 0 {
		cfg.Slack.AdapterConfig.AllowedChannelIDs = getEnvList("SLACK_ALLOWED_CHANNEL_IDS", []string{})
	}
	if len(cfg.Slack.AdapterConfig.AllowedUserIDs) == 0 {
		cfg.Slack.AdapterConfig.AllowedUserIDs = getEnvList("SLACK_ALLOWED_USER_IDS", []string{})
	}

	socketMode := derefBool(cfg.Slack.AdapterConfig.SocketMode, true)
	autoThread := derefBool(cfg.Slack.AdapterConfig.AutoThread, true)

	if cfg.Slack.Enabled {
		if cfg.Slack.Credentials.BotToken == "" {
			return nil, fmt.Errorf("SLACK_BOT_TOKEN is required when Slack is enabled")
		}
		if socketMode && cfg.Slack.Credentials.AppToken == "" {
			return nil, fmt.Errorf("SLACK_APP_TOKEN is required for Socket Mode")
		}
	}

	cfg.Slack.Config = adapter.Config{
		BotToken:            cfg.Slack.Credentials.BotToken,
		AppToken:            cfg.Slack.Credentials.AppToken,
		SocketMode:          socketMode,
		AutoThread:          autoThread,
		DevMode:             getEnvBool("DEV", false),
		ActionableReactions: cfg.Slack.AdapterConfig.ActionableReactions,
		AllowedChannelIDs:   cfg.Slack.AdapterConfig.AllowedChannelIDs,
		AllowedUserIDs:      cfg.Slack.AdapterConfig.AllowedUserIDs,
		RateLimit: adapter.RateLimitConfig{
			RequestsPerSecond: getEnvFloat("SLACK_RATE_LIMIT_RPS", 3.0),
			BurstSize:         getEnvInt("SLACK_RATE_LIMIT_BURST", 10),
		},
	}

	// Web configuration
	cfg.Web = WebConfig{
		Enabled:         getEnvBool("WEB_ENABLED", false),
		ListenAddr:      getEnv("WEB_LISTEN_ADDR", ":8080"),
		AllowedOrigins:  getEnvList("WEB_ALLOWED_ORIGINS", []string{"*"}),
		ServePlayground: getEnvBool("WEB_SERVE_PLAYGROUND", false),
	}

	// Metrics configuration
	cfg.Metrics = MetricsConfig{
		Enabled:    getEnvBool("METRICS_ENABLED", true),
		ListenAddr: getEnv("METRICS_LISTEN_ADDR", ":9091"),
	}

	// Storage configuration
	cfg.Storage = StorageConfig{
		Type:     getEnv("STORAGE_TYPE", "redis"),
		RedisURL: getEnv("REDIS_URL", "redis://localhost:6379"),
		TTL:      getEnvInt("STORAGE_TTL", 604800), // 7 days default
	}

	// Authorization configuration. The token is wired in by astro-server's
	// K8s spec applier; an empty value means we're running outside that
	// wiring (local dev) and skip the check.
	cfg.Authz = AuthzConfig{
		IdentityToken: os.Getenv("ASTRO_AUTHZ_TOKEN"),
	}

	return cfg, nil
}

// Helper functions

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvBool(key string, defaultValue bool) bool {
	if value := os.Getenv(key); value != "" {
		b, err := strconv.ParseBool(value)
		if err == nil {
			return b
		}
	}
	return defaultValue
}

func getEnvInt(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		i, err := strconv.Atoi(value)
		if err == nil {
			return i
		}
	}
	return defaultValue
}

func getEnvList(key string, defaultValue []string) []string {
	if value := os.Getenv(key); value != "" {
		parts := strings.Split(value, ",")
		result := make([]string, 0, len(parts))
		for _, p := range parts {
			if trimmed := strings.TrimSpace(p); trimmed != "" {
				result = append(result, trimmed)
			}
		}
		if len(result) > 0 {
			return result
		}
	}
	return defaultValue
}

func getEnvFloat(key string, defaultValue float64) float64 {
	if value := os.Getenv(key); value != "" {
		f, err := strconv.ParseFloat(value, 64)
		if err == nil {
			return f
		}
	}
	return defaultValue
}

func boolPtr(v bool) *bool { return &v }

func derefBool(p *bool, fallback bool) bool {
	if p != nil {
		return *p
	}
	return fallback
}
