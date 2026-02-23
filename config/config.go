package config

import (
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

	// MCP (Model Context Protocol) configuration
	MCP MCPConfig

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

// SlackConfig holds Slack-specific configuration
type SlackConfig struct {
	Enabled    bool
	BotToken   string
	AppToken   string
	SocketMode bool
	AutoThread bool
	Config     adapter.Config
}

// WebConfig holds web adapter configuration
type WebConfig struct {
	Enabled        bool
	ListenAddr     string
	AllowedOrigins []string
}

// MCPConfig holds MCP (Model Context Protocol) adapter configuration
type MCPConfig struct {
	Enabled    bool
	ListenAddr string
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

	// Slack configuration
	cfg.Slack = SlackConfig{
		Enabled:    getEnvBool("SLACK_ENABLED", false),
		BotToken:   getEnv("SLACK_BOT_TOKEN", ""),
		AppToken:   getEnv("SLACK_APP_TOKEN", ""),
		SocketMode: getEnvBool("SLACK_SOCKET_MODE", true),
		AutoThread: getEnvBool("SLACK_AUTO_THREAD", true),
	}

	// Validate Slack configuration if enabled
	if cfg.Slack.Enabled {
		if cfg.Slack.BotToken == "" {
			return nil, fmt.Errorf("SLACK_BOT_TOKEN is required when Slack is enabled")
		}
		if cfg.Slack.SocketMode && cfg.Slack.AppToken == "" {
			return nil, fmt.Errorf("SLACK_APP_TOKEN is required for Socket Mode")
		}
	}

	// Set adapter config
	cfg.Slack.Config = adapter.Config{
		BotToken:   cfg.Slack.BotToken,
		AppToken:   cfg.Slack.AppToken,
		SocketMode: cfg.Slack.SocketMode,
		AutoThread: cfg.Slack.AutoThread,
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

	// MCP configuration
	cfg.MCP = MCPConfig{
		Enabled:    getEnvBool("MCP_ENABLED", false),
		ListenAddr: getEnv("MCP_LISTEN_ADDR", ":8081"),
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
