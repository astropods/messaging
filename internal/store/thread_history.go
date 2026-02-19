package store

import (
	"sync"
	"time"

	pb "github.com/astromode-ai/astro-messaging/pkg/gen/astro/messaging/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// ThreadHistoryStore stores recent platform thread messages for all adapters
// This provides ground truth for platform thread state (handles edits/deletes)
type ThreadHistoryStore struct {
	threads    map[string]*ThreadHistory
	mu         sync.RWMutex
	maxSize    int           // Max number of threads to store
	maxMessages int           // Max messages per thread
	ttl        time.Duration
}

// ThreadHistory represents a conversation thread with its messages
type ThreadHistory struct {
	ConversationID string
	Messages       []*pb.ThreadMessage
	LastFetched    time.Time
	Platform       string
}

// NewThreadHistoryStore creates a new thread history store
func NewThreadHistoryStore(maxSize, maxMessages int, ttl time.Duration) *ThreadHistoryStore {
	return &ThreadHistoryStore{
		threads:     make(map[string]*ThreadHistory),
		maxSize:     maxSize,
		maxMessages: maxMessages,
		ttl:         ttl,
	}
}

// AddMessage stores a message in thread history
func (s *ThreadHistoryStore) AddMessage(conversationID string, msg *pb.ThreadMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()

	thread, exists := s.threads[conversationID]
	if !exists {
		// Create new thread
		thread = &ThreadHistory{
			ConversationID: conversationID,
			Messages:       make([]*pb.ThreadMessage, 0, s.maxMessages),
			LastFetched:    time.Now(),
		}
		s.threads[conversationID] = thread

		// Evict oldest thread if at capacity
		if len(s.threads) > s.maxSize {
			s.evictOldest()
		}
	}

	// Add message
	thread.Messages = append(thread.Messages, msg)

	// Keep only last N messages
	if len(thread.Messages) > s.maxMessages {
		thread.Messages = thread.Messages[len(thread.Messages)-s.maxMessages:]
	}

	thread.LastFetched = time.Now()
}

// UpdateMessage handles message edits
func (s *ThreadHistoryStore) UpdateMessage(conversationID string, messageID string, newContent string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	thread, exists := s.threads[conversationID]
	if !exists {
		return false
	}

	for _, msg := range thread.Messages {
		if msg.MessageId == messageID {
			if !msg.WasEdited {
				// Store original content before first edit
				msg.OriginalContent = msg.Content
				msg.WasEdited = true
			}
			msg.Content = newContent
			msg.EditedAt = timestamppb.Now()
			return true
		}
	}

	return false
}

// DeleteMessage marks a message as deleted
func (s *ThreadHistoryStore) DeleteMessage(conversationID string, messageID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	thread, exists := s.threads[conversationID]
	if !exists {
		return false
	}

	for _, msg := range thread.Messages {
		if msg.MessageId == messageID {
			msg.IsDeleted = true
			msg.DeletedAt = timestamppb.Now()
			return true
		}
	}

	return false
}

// GetHistory returns thread history for a conversation
func (s *ThreadHistoryStore) GetHistory(conversationID string, maxMessages int, includeDeleted bool) *pb.ThreadHistoryResponse {
	s.mu.RLock()
	defer s.mu.RUnlock()

	thread, exists := s.threads[conversationID]
	if !exists {
		return &pb.ThreadHistoryResponse{
			ConversationId: conversationID,
			Messages:       []*pb.ThreadMessage{},
			IsComplete:     true,
			FetchedAt:      timestamppb.Now(),
		}
	}

	// Filter messages
	messages := thread.Messages
	if !includeDeleted {
		filtered := make([]*pb.ThreadMessage, 0, len(messages))
		for _, msg := range messages {
			if !msg.IsDeleted {
				filtered = append(filtered, msg)
			}
		}
		messages = filtered
	}

	// Limit to maxMessages
	if len(messages) > maxMessages {
		messages = messages[len(messages)-maxMessages:]
	}

	return &pb.ThreadHistoryResponse{
		ConversationId: conversationID,
		Messages:       messages,
		IsComplete:     len(messages) <= maxMessages,
		FetchedAt:      timestamppb.New(thread.LastFetched),
	}
}

// IsStale checks if thread history needs refresh
func (s *ThreadHistoryStore) IsStale(conversationID string, staleDuration time.Duration) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	thread, exists := s.threads[conversationID]
	if !exists {
		return true
	}

	return time.Since(thread.LastFetched) > staleDuration
}

// Exists checks if a conversation exists in the store
func (s *ThreadHistoryStore) Exists(conversationID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	_, exists := s.threads[conversationID]
	return exists
}

// Clear removes a thread from the store
func (s *ThreadHistoryStore) Clear(conversationID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.threads, conversationID)
}

// evictOldest removes the oldest thread (must be called with lock held)
func (s *ThreadHistoryStore) evictOldest() {
	var oldestID string
	var oldestTime time.Time

	for id, thread := range s.threads {
		if oldestID == "" || thread.LastFetched.Before(oldestTime) {
			oldestID = id
			oldestTime = thread.LastFetched
		}
	}

	if oldestID != "" {
		delete(s.threads, oldestID)
	}
}

// CleanupStale removes threads older than TTL
func (s *ThreadHistoryStore) CleanupStale() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	var removed int
	now := time.Now()

	for id, thread := range s.threads {
		if now.Sub(thread.LastFetched) > s.ttl {
			delete(s.threads, id)
			removed++
		}
	}

	return removed
}

// Stats returns store statistics
func (s *ThreadHistoryStore) Stats() ThreadHistoryStats {
	s.mu.RLock()
	defer s.mu.RUnlock()

	stats := ThreadHistoryStats{
		TotalThreads: len(s.threads),
	}

	for _, thread := range s.threads {
		stats.TotalMessages += len(thread.Messages)
	}

	return stats
}

// ThreadHistoryStats provides statistics about the store
type ThreadHistoryStats struct {
	TotalThreads  int
	TotalMessages int
}
