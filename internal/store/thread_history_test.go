package store

import (
	"testing"
	"time"

	pb "github.com/astromode-ai/astro-messaging/pkg/gen/astro/messaging/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestThreadHistoryStore_AddMessage(t *testing.T) {
	store := NewThreadHistoryStore(100, 50, time.Hour)

	msg := &pb.ThreadMessage{
		MessageId: "msg1",
		User: &pb.User{
			Id:       "U123",
			Username: "alice",
		},
		Content:   "Hello world",
		Timestamp: timestamppb.Now(),
	}

	store.AddMessage("conv1", msg)

	history := store.GetHistory("conv1", 10, false)
	if len(history.Messages) != 1 {
		t.Errorf("Expected 1 message, got %d", len(history.Messages))
	}

	if history.Messages[0].Content != "Hello world" {
		t.Errorf("Expected content 'Hello world', got '%s'", history.Messages[0].Content)
	}
}

func TestThreadHistoryStore_UpdateMessage(t *testing.T) {
	store := NewThreadHistoryStore(100, 50, time.Hour)

	// Add initial message
	msg := &pb.ThreadMessage{
		MessageId: "msg1",
		User: &pb.User{
			Id:       "U123",
			Username: "alice",
		},
		Content:   "What's the weather?",
		Timestamp: timestamppb.Now(),
	}

	store.AddMessage("conv1", msg)

	// Edit the message
	updated := store.UpdateMessage("conv1", "msg1", "What's the weather in Paris?")
	if !updated {
		t.Error("Expected message to be updated")
	}

	// Get history and verify
	history := store.GetHistory("conv1", 10, false)
	if len(history.Messages) != 1 {
		t.Fatalf("Expected 1 message, got %d", len(history.Messages))
	}

	msg = history.Messages[0]
	if !msg.WasEdited {
		t.Error("Expected message to be marked as edited")
	}

	if msg.Content != "What's the weather in Paris?" {
		t.Errorf("Expected edited content, got '%s'", msg.Content)
	}

	if msg.OriginalContent != "What's the weather?" {
		t.Errorf("Expected original content to be preserved, got '%s'", msg.OriginalContent)
	}

	if msg.EditedAt == nil {
		t.Error("Expected EditedAt to be set")
	}
}

func TestThreadHistoryStore_DeleteMessage(t *testing.T) {
	store := NewThreadHistoryStore(100, 50, time.Hour)

	// Add message
	msg := &pb.ThreadMessage{
		MessageId: "msg1",
		User: &pb.User{
			Id:       "U123",
			Username: "alice",
		},
		Content:   "Test message",
		Timestamp: timestamppb.Now(),
	}

	store.AddMessage("conv1", msg)

	// Delete the message
	deleted := store.DeleteMessage("conv1", "msg1")
	if !deleted {
		t.Error("Expected message to be deleted")
	}

	// Get history with deleted messages
	history := store.GetHistory("conv1", 10, true)
	if len(history.Messages) != 1 {
		t.Fatalf("Expected 1 message (including deleted), got %d", len(history.Messages))
	}

	if !history.Messages[0].IsDeleted {
		t.Error("Expected message to be marked as deleted")
	}

	// Get history without deleted messages
	history = store.GetHistory("conv1", 10, false)
	if len(history.Messages) != 0 {
		t.Errorf("Expected 0 messages (excluding deleted), got %d", len(history.Messages))
	}
}

func TestThreadHistoryStore_MaxMessages(t *testing.T) {
	store := NewThreadHistoryStore(100, 5, time.Hour) // Only keep 5 messages

	// Add 10 messages
	for i := 0; i < 10; i++ {
		msg := &pb.ThreadMessage{
			MessageId: string(rune('a' + i)),
			User: &pb.User{
				Id:       "U123",
				Username: "alice",
			},
			Content:   "Message " + string(rune('a'+i)),
			Timestamp: timestamppb.Now(),
		}
		store.AddMessage("conv1", msg)
	}

	// Should only have last 5 messages
	history := store.GetHistory("conv1", 100, false)
	if len(history.Messages) != 5 {
		t.Errorf("Expected 5 messages, got %d", len(history.Messages))
	}

	// First message should be 'f' (6th message)
	if history.Messages[0].MessageId != "f" {
		t.Errorf("Expected first message ID 'f', got '%s'", history.Messages[0].MessageId)
	}
}

func TestThreadHistoryStore_IsStale(t *testing.T) {
	store := NewThreadHistoryStore(100, 50, time.Hour)

	// Non-existent conversation is stale
	if !store.IsStale("conv1", 5*time.Minute) {
		t.Error("Expected non-existent conversation to be stale")
	}

	// Add message
	msg := &pb.ThreadMessage{
		MessageId: "msg1",
		User: &pb.User{
			Id:       "U123",
			Username: "alice",
		},
		Content:   "Test",
		Timestamp: timestamppb.Now(),
	}

	store.AddMessage("conv1", msg)

	// Should not be stale immediately
	if store.IsStale("conv1", 5*time.Minute) {
		t.Error("Expected fresh conversation to not be stale")
	}

	// Manually set LastFetched to old time
	store.mu.Lock()
	store.threads["conv1"].LastFetched = time.Now().Add(-10 * time.Minute)
	store.mu.Unlock()

	// Should now be stale
	if !store.IsStale("conv1", 5*time.Minute) {
		t.Error("Expected old conversation to be stale")
	}
}

func TestThreadHistoryStore_CleanupStale(t *testing.T) {
	store := NewThreadHistoryStore(100, 50, time.Hour)

	// Add some messages
	for i := 0; i < 3; i++ {
		msg := &pb.ThreadMessage{
			MessageId: string(rune('a' + i)),
			User: &pb.User{
				Id:       "U123",
				Username: "alice",
			},
			Content:   "Message",
			Timestamp: timestamppb.Now(),
		}
		store.AddMessage("conv"+string(rune('1'+i)), msg)
	}

	// Make one stale
	store.mu.Lock()
	store.threads["conv1"].LastFetched = time.Now().Add(-2 * time.Hour)
	store.mu.Unlock()

	// Cleanup
	removed := store.CleanupStale()
	if removed != 1 {
		t.Errorf("Expected 1 thread removed, got %d", removed)
	}

	// Verify
	if store.Exists("conv1") {
		t.Error("Expected stale thread to be removed")
	}

	if !store.Exists("conv2") || !store.Exists("conv3") {
		t.Error("Expected fresh threads to remain")
	}
}

func TestThreadHistoryStore_Stats(t *testing.T) {
	store := NewThreadHistoryStore(100, 50, time.Hour)

	// Add messages to multiple threads
	for i := 0; i < 3; i++ {
		for j := 0; j < 2; j++ {
			msg := &pb.ThreadMessage{
				MessageId: string(rune('a' + j)),
				User: &pb.User{
					Id:       "U123",
					Username: "alice",
				},
				Content:   "Message",
				Timestamp: timestamppb.Now(),
			}
			store.AddMessage("conv"+string(rune('1'+i)), msg)
		}
	}

	stats := store.Stats()
	if stats.TotalThreads != 3 {
		t.Errorf("Expected 3 threads, got %d", stats.TotalThreads)
	}

	if stats.TotalMessages != 6 {
		t.Errorf("Expected 6 messages, got %d", stats.TotalMessages)
	}
}

func TestThreadHistoryStore_Eviction(t *testing.T) {
	store := NewThreadHistoryStore(3, 50, time.Hour) // Max 3 threads

	// Add 4 threads
	for i := 0; i < 4; i++ {
		msg := &pb.ThreadMessage{
			MessageId: string(rune('a' + i)),
			User: &pb.User{
				Id:       "U123",
				Username: "alice",
			},
			Content:   "Message",
			Timestamp: timestamppb.Now(),
		}

		// Add delay to ensure different timestamps
		time.Sleep(10 * time.Millisecond)
		store.AddMessage("conv"+string(rune('1'+i)), msg)
	}

	// Should only have 3 threads (oldest evicted)
	stats := store.Stats()
	if stats.TotalThreads != 3 {
		t.Errorf("Expected 3 threads after eviction, got %d", stats.TotalThreads)
	}

	// First thread (oldest) should be evicted
	if store.Exists("conv1") {
		t.Error("Expected oldest thread to be evicted")
	}

	// Newer threads should exist
	if !store.Exists("conv2") || !store.Exists("conv3") || !store.Exists("conv4") {
		t.Error("Expected newer threads to be retained")
	}
}
