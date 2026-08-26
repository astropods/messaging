package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// savedConversationNamespace seeds the UUIDv5 derivation below. It is fixed
// forever: changing it orphans every conversation already saved under the old
// value, since the id is the only link back to the agent's idempotency key.
var savedConversationNamespace = uuid.MustParse("8f2b0a54-6d31-4c9e-9a77-1f0c5b83e2d1")

// DeriveSavedConversationID maps an agent's (user, idempotency key) pair to the
// conversation id its saves land on. Deriving rather than allocating lets the
// agent link to the copy without a round trip.
func DeriveSavedConversationID(userID, idempotencyKey string) string {
	return uuid.NewSHA1(savedConversationNamespace, []byte(userID+"\x00"+idempotencyKey)).String()
}

// SavedMessage is one turn of an external conversation being copied in.
type SavedMessage struct {
	Role      string
	Author    string
	Content   string
	Timestamp time.Time
}

// SaveConversation copies an external conversation into userID's history under
// the id derived from idempotencyKey, replacing whatever a previous save under
// the same key wrote.
//
// Reports false when the user deleted this copy. A delete is terminal, so an
// agent that saves on every source message cannot resurrect it.
func (s *Store) SaveConversation(
	ctx context.Context,
	userID, idempotencyKey, title, sourceLabel, sourceURL string,
	msgs []SavedMessage,
) (conversationID string, saved bool, err error) {
	if userID == "" || idempotencyKey == "" {
		return "", false, errors.New("chatstore save: user id and idempotency key are required")
	}
	conversationID = DeriveSavedConversationID(userID, idempotencyKey)
	if len(msgs) > MaxMessagesPerConversation {
		msgs = msgs[len(msgs)-MaxMessagesPerConversation:]
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", false, fmt.Errorf("chatstore save begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful commit

	now := time.Now().UnixMilli()
	var (
		owner     string
		deletedAt sql.NullInt64
	)
	err = tx.QueryRowContext(ctx,
		`SELECT user_id, deleted_at FROM conversations WHERE conversation_id = ?`,
		conversationID,
	).Scan(&owner, &deletedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO conversations
				(conversation_id, user_id, title, created_at, updated_at, source_label, source_url)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			conversationID, userID, title, now, now, sourceLabel, sourceURL,
		); err != nil {
			return "", false, fmt.Errorf("chatstore save insert: %w", err)
		}
	case err != nil:
		return "", false, fmt.Errorf("chatstore save read: %w", err)
	case deletedAt.Valid:
		return conversationID, false, nil
	// A derived id embeds userID, so a foreign owner means this id collided with
	// one allocated elsewhere. Refuse rather than overwrite someone's own chat.
	case owner != userID:
		return "", false, nil
	default:
		if _, err := tx.ExecContext(ctx, `
			UPDATE conversations
			SET title = ?, updated_at = ?, source_label = ?, source_url = ?
			WHERE conversation_id = ?`,
			title, now, sourceLabel, sourceURL, conversationID,
		); err != nil {
			return "", false, fmt.Errorf("chatstore save update: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM messages WHERE conversation_id = ?`, conversationID,
		); err != nil {
			return "", false, fmt.Errorf("chatstore save clear: %w", err)
		}
	}

	for i, m := range msgs {
		created := now
		if !m.Timestamp.IsZero() {
			created = m.Timestamp.UnixMilli()
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO messages
				(id, conversation_id, user_id, role, content, seq, created_at, attachments, author)
			VALUES (?, ?, ?, ?, ?, ?, ?, '', ?)`,
			uuid.NewString(), conversationID, userID, m.Role,
			TruncateRunes(m.Content, MaxMessageContentRunes), i+1, created, m.Author,
		); err != nil {
			return "", false, fmt.Errorf("chatstore save message: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return "", false, fmt.Errorf("chatstore save commit: %w", err)
	}
	return conversationID, true, nil
}
