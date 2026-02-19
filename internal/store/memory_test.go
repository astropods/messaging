package store

import (
	"context"
	"testing"
	"time"

	"github.com/astromode-ai/astro-messaging/pkg/types"
)

func TestMemoryStore_CreateAndGet(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()

	// Create a conversation
	conversation := &types.ConversationContext{
		ConversationID: "test-conversation-1",
		Platform:       "slack",
		ChannelID:      "C123456",
		UserID:         "U123456",
		CreatedAt:      time.Now(),
		LastMessageAt:  time.Now(),
		MessageCount:   0,
		Metadata:       map[string]any{"test": "data"},
	}

	err := store.Create(ctx, conversation)
	if err != nil {
		t.Fatalf("Failed to create conversation: %v", err)
	}

	// Retrieve the conversation
	retrieved, err := store.Get(ctx, "test-conversation-1")
	if err != nil {
		t.Fatalf("Failed to get conversation: %v", err)
	}

	if retrieved.ConversationID != conversation.ConversationID {
		t.Errorf("Expected conversation ID %s, got %s", conversation.ConversationID, retrieved.ConversationID)
	}

	if retrieved.Platform != conversation.Platform {
		t.Errorf("Expected platform %s, got %s", conversation.Platform, retrieved.Platform)
	}
}

func TestMemoryStore_CreateDuplicate(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()

	conversation := &types.ConversationContext{
		ConversationID: "test-conversation-2",
		Platform:       "slack",
		ChannelID:      "C123456",
		UserID:         "U123456",
		CreatedAt:      time.Now(),
		LastMessageAt:  time.Now(),
		MessageCount:   0,
		Metadata:       map[string]any{},
	}

	// Create once
	err := store.Create(ctx, conversation)
	if err != nil {
		t.Fatalf("Failed to create conversation: %v", err)
	}

	// Try to create again (should fail)
	err = store.Create(ctx, conversation)
	if err == nil {
		t.Error("Expected error when creating duplicate conversation")
	}
}

func TestMemoryStore_Update(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()

	conversation := &types.ConversationContext{
		ConversationID: "test-conversation-3",
		Platform:       "slack",
		ChannelID:      "C123456",
		UserID:         "U123456",
		CreatedAt:      time.Now(),
		LastMessageAt:  time.Now(),
		MessageCount:   0,
		Metadata:       map[string]any{},
	}

	// Create
	err := store.Create(ctx, conversation)
	if err != nil {
		t.Fatalf("Failed to create conversation: %v", err)
	}

	// Update
	conversation.MessageCount = 5
	conversation.LastMessageAt = time.Now()

	err = store.Update(ctx, conversation)
	if err != nil {
		t.Fatalf("Failed to update conversation: %v", err)
	}

	// Retrieve and verify
	retrieved, err := store.Get(ctx, "test-conversation-3")
	if err != nil {
		t.Fatalf("Failed to get conversation: %v", err)
	}

	if retrieved.MessageCount != 5 {
		t.Errorf("Expected message count 5, got %d", retrieved.MessageCount)
	}
}

func TestMemoryStore_Delete(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()

	conversation := &types.ConversationContext{
		ConversationID: "test-conversation-4",
		Platform:       "slack",
		ChannelID:      "C123456",
		UserID:         "U123456",
		CreatedAt:      time.Now(),
		LastMessageAt:  time.Now(),
		MessageCount:   0,
		Metadata:       map[string]any{},
	}

	// Create
	err := store.Create(ctx, conversation)
	if err != nil {
		t.Fatalf("Failed to create conversation: %v", err)
	}

	// Delete
	err = store.Delete(ctx, "test-conversation-4")
	if err != nil {
		t.Fatalf("Failed to delete conversation: %v", err)
	}

	// Try to get (should fail)
	_, err = store.Get(ctx, "test-conversation-4")
	if err == nil {
		t.Error("Expected error when getting deleted conversation")
	}
}

func TestMemoryStore_GetNonExistent(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()

	_, err := store.Get(ctx, "non-existent")
	if err == nil {
		t.Error("Expected error when getting non-existent conversation")
	}
}
