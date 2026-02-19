package store

import (
	"context"

	"github.com/astromode-ai/astro-messaging/pkg/types"
)

// ConversationStore is the interface for storing conversation contexts
type ConversationStore interface {
	// Get retrieves a conversation context by ID
	Get(ctx context.Context, conversationID string) (*types.ConversationContext, error)

	// Create creates a new conversation context
	Create(ctx context.Context, context *types.ConversationContext) error

	// Update updates an existing conversation context
	Update(ctx context.Context, context *types.ConversationContext) error

	// Delete deletes a conversation context
	Delete(ctx context.Context, conversationID string) error

	// Close closes the store connection
	Close() error
}
