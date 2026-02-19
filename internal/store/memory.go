package store

import (
	"context"
	"fmt"
	"sync"

	"github.com/astromode-ai/astro-messaging/pkg/types"
)

// MemoryStore is an in-memory implementation of ConversationStore
// Useful for development and testing. Data is lost when the process stops.
type MemoryStore struct {
	conversations map[string]*types.ConversationContext
	mu            sync.RWMutex
}

// NewMemoryStore creates a new in-memory conversation store
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		conversations: make(map[string]*types.ConversationContext),
	}
}

// Get retrieves a conversation context by ID
func (s *MemoryStore) Get(ctx context.Context, conversationID string) (*types.ConversationContext, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	context, ok := s.conversations[conversationID]
	if !ok {
		return nil, fmt.Errorf("conversation not found: %s", conversationID)
	}

	// Return a copy to prevent external modifications
	contextCopy := *context
	return &contextCopy, nil
}

// Create creates a new conversation context
func (s *MemoryStore) Create(ctx context.Context, context *types.ConversationContext) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.conversations[context.ConversationID]; exists {
		return fmt.Errorf("conversation already exists: %s", context.ConversationID)
	}

	// Store a copy
	contextCopy := *context
	s.conversations[context.ConversationID] = &contextCopy

	return nil
}

// Update updates an existing conversation context
func (s *MemoryStore) Update(ctx context.Context, context *types.ConversationContext) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.conversations[context.ConversationID]; !exists {
		return fmt.Errorf("conversation not found: %s", context.ConversationID)
	}

	// Store a copy
	contextCopy := *context
	s.conversations[context.ConversationID] = &contextCopy

	return nil
}

// Delete deletes a conversation context
func (s *MemoryStore) Delete(ctx context.Context, conversationID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.conversations[conversationID]; !exists {
		return fmt.Errorf("conversation not found: %s", conversationID)
	}

	delete(s.conversations, conversationID)
	return nil
}

// Close closes the store (no-op for memory store)
func (s *MemoryStore) Close() error {
	return nil
}
