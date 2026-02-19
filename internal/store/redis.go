package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/astromode-ai/astro-messaging/pkg/types"
	"github.com/redis/go-redis/v9"
)

// RedisStore is a Redis-backed implementation of ConversationStore
type RedisStore struct {
	client *redis.Client
	ttl    time.Duration
}

// NewRedisStore creates a new Redis conversation store
func NewRedisStore(redisURL string, ttlSeconds int) (*RedisStore, error) {
	// Parse Redis URL
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse Redis URL: %w", err)
	}

	// Create Redis client
	client := redis.NewClient(opts)

	// Test connection
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("failed to connect to Redis: %w", err)
	}

	return &RedisStore{
		client: client,
		ttl:    time.Duration(ttlSeconds) * time.Second,
	}, nil
}

// Get retrieves a conversation context by ID
func (s *RedisStore) Get(ctx context.Context, conversationID string) (*types.ConversationContext, error) {
	key := fmt.Sprintf("conversation:%s", conversationID)

	data, err := s.client.Get(ctx, key).Bytes()
	if err == redis.Nil {
		return nil, fmt.Errorf("conversation not found: %s", conversationID)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get conversation: %w", err)
	}

	var context types.ConversationContext
	if err := json.Unmarshal(data, &context); err != nil {
		return nil, fmt.Errorf("failed to unmarshal conversation: %w", err)
	}

	return &context, nil
}

// Create creates a new conversation context
func (s *RedisStore) Create(ctx context.Context, context *types.ConversationContext) error {
	key := fmt.Sprintf("conversation:%s", context.ConversationID)

	// Check if already exists
	exists, err := s.client.Exists(ctx, key).Result()
	if err != nil {
		return fmt.Errorf("failed to check conversation existence: %w", err)
	}
	if exists > 0 {
		return fmt.Errorf("conversation already exists: %s", context.ConversationID)
	}

	// Marshal to JSON
	data, err := json.Marshal(context)
	if err != nil {
		return fmt.Errorf("failed to marshal conversation: %w", err)
	}

	// Store with TTL
	if err := s.client.SetEx(ctx, key, data, s.ttl).Err(); err != nil {
		return fmt.Errorf("failed to create conversation: %w", err)
	}

	return nil
}

// Update updates an existing conversation context
func (s *RedisStore) Update(ctx context.Context, context *types.ConversationContext) error {
	key := fmt.Sprintf("conversation:%s", context.ConversationID)

	// Marshal to JSON
	data, err := json.Marshal(context)
	if err != nil {
		return fmt.Errorf("failed to marshal conversation: %w", err)
	}

	// Update with TTL (upsert behavior)
	if err := s.client.SetEx(ctx, key, data, s.ttl).Err(); err != nil {
		return fmt.Errorf("failed to update conversation: %w", err)
	}

	return nil
}

// Delete deletes a conversation context
func (s *RedisStore) Delete(ctx context.Context, conversationID string) error {
	key := fmt.Sprintf("conversation:%s", conversationID)

	result, err := s.client.Del(ctx, key).Result()
	if err != nil {
		return fmt.Errorf("failed to delete conversation: %w", err)
	}
	if result == 0 {
		return fmt.Errorf("conversation not found: %s", conversationID)
	}

	return nil
}

// Close closes the Redis connection
func (s *RedisStore) Close() error {
	return s.client.Close()
}
