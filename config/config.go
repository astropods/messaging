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

	// Logging
	LogLevel string
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
	AllowedChannels     []string `json:"allowed_channels,omitempty"`
	AllowedUsers        []string `json:"allowed_users,omitempty"`
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
	Enabled        bool
	ListenAddr     string
	AllowedOrigins []string
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
	if len(cfg.Slack.AdapterConfig.AllowedChannels) == 0 {
		cfg.Slack.AdapterConfig.AllowedChannels = getEnvList("SLACK_ALLOWED_CHANNELS", []string{})
	}
	if len(cfg.Slack.AdapterConfig.AllowedUsers) == 0 {
		cfg.Slack.AdapterConfig.AllowedUsers = getEnvList("SLACK_ALLOWED_USERS", []string{})
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
		ActionableReactions: cfg.Slack.AdapterConfig.ActionableReactions,
		AllowedChannelIDs:   cfg.Slack.AdapterConfig.AllowedChannels,
		AllowedUserIDs:      cfg.Slack.AdapterConfig.AllowedUsers,
		RateLimit: adapter.RateLimitConfig{
			RequestsPerSecond: getEnvFloat("SLACK_RATE_LIMIT_RPS", 3.0),
			BurstSize:         getEnvInt("SLACK_RATE_LIMIT_BURST", 10),
		},
	}

	// Web configuration
	cfg.Web = WebConfig{
		Enabled:        getEnvBool("WEB_ENABLED", false),
		ListenAddr:     getEnv("WEB_LISTEN_ADDR", ":8080"),
		AllowedOrigins: getEnvList("WEB_ALLOWED_ORIGINS", []string{"*"}),
	}

	// Storage configuration
	cfg.Storage = StorageConfig{
		Type:     getEnv("STORAGE_TYPE", "redis"),
		RedisURL: getEnv("REDIS_URL", "redis://localhost:6379"),
		TTL:      getEnvInt("STORAGE_TTL", 604800), // 7 days default
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
