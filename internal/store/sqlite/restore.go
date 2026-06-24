package sqlite

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// RestoredMessage is a message rebuilt from Langfuse for backfill.
type RestoredMessage struct {
	Role    string
	Content string
}

// RestoredConversation is a conversation summary rebuilt from Langfuse for backfill.
type RestoredConversation struct {
	ConversationID string
	Title          string
	UpdatedAt      time.Time
}

// BackfillMessages inserts a rebuilt thread for a conversation, but only when it
// currently has no local messages. This makes restore idempotent: once the
// thread exists locally, new turns append normally and Langfuse is not consulted
// again. The conversation row is created if missing.
func (s *Store) BackfillMessages(conversationID, userID, title string, msgs []RestoredMessage) error {
	if len(msgs) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("chatstore backfill begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM messages WHERE conversation_id = ?`, conversationID).Scan(&count); err != nil {
		return fmt.Errorf("chatstore backfill count: %w", err)
	}
	if count > 0 {
		return nil
	}

	now := time.Now().UnixMilli()
	if _, err := tx.Exec(`
		INSERT INTO conversations (conversation_id, user_id, title, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(conversation_id) DO UPDATE SET deleted_at = NULL`,
		conversationID, userID, title, now, now,
	); err != nil {
		return fmt.Errorf("chatstore backfill conversation: %w", err)
	}

	for i, m := range msgs {
		content := truncateRunes(m.Content, MaxMessageContentRunes)
		if _, err := tx.Exec(`
			INSERT INTO messages (id, conversation_id, user_id, role, content, seq, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			uuid.NewString(), conversationID, userID, m.Role, content, i+1, now,
		); err != nil {
			return fmt.Errorf("chatstore backfill message: %w", err)
		}
	}
	return tx.Commit()
}

// BackfillConversations inserts conversation rows that are missing locally,
// preserving Langfuse recency. Existing rows are left untouched.
func (s *Store) BackfillConversations(userID string, convs []RestoredConversation) error {
	for _, c := range convs {
		updated := c.UpdatedAt.UnixMilli()
		if c.UpdatedAt.IsZero() {
			updated = time.Now().UnixMilli()
		}
		if _, err := s.db.Exec(`
			INSERT INTO conversations (conversation_id, user_id, title, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(conversation_id) DO NOTHING`,
			c.ConversationID, userID, c.Title, updated, updated,
		); err != nil {
			return fmt.Errorf("chatstore backfill conversations: %w", err)
		}
	}
	return nil
}
