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

// originSave marks the message rows a save wrote, so a later save can tell them
// from turns the user typed into the copy themselves.
const originSave = "save"

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

// OnConflict selects what a save does to a copy that already exists.
type OnConflict int

const (
	// OnConflictSkip refreshes a copy the user has not touched and leaves a
	// diverged one alone. The safe default: it never destroys turns the user wrote.
	OnConflictSkip OnConflict = iota
	// OnConflictReplace overwrites regardless, discarding the user's own turns.
	OnConflictReplace
	// OnConflictAppend adds the given messages after whatever is already there.
	OnConflictAppend
)

// SaveStatus reports what a save did, so the agent can decide what to do next.
type SaveStatus string

const (
	SaveCreated         SaveStatus = "created"
	SaveReplaced        SaveStatus = "replaced"
	SaveAppended        SaveStatus = "appended"
	SaveSkippedDeleted  SaveStatus = "skipped_deleted"
	SaveSkippedDiverged SaveStatus = "skipped_diverged"
	SaveSkippedConflict SaveStatus = "skipped_conflict"
)

// SaveRequest copies an external conversation into one user's chat history.
type SaveRequest struct {
	UserID         string
	IdempotencyKey string
	Title          string
	SourceLabel    string
	SourceURL      string
	Messages       []SavedMessage
	OnConflict     OnConflict
}

// SaveConversation writes an external conversation into the requesting user's
// history under the id derived from the idempotency key, and reports what it
// did. The status is the point of the call: a copy can be deleted, diverged, or
// owned by someone else, and only the agent can decide how to react.
func (s *Store) SaveConversation(ctx context.Context, req SaveRequest) (string, SaveStatus, error) {
	if req.UserID == "" || req.IdempotencyKey == "" {
		return "", "", errors.New("chatstore save: user id and idempotency key are required")
	}
	conversationID := DeriveSavedConversationID(req.UserID, req.IdempotencyKey)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", "", fmt.Errorf("chatstore save begin: %w", err)
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

	status := SaveCreated
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO conversations
				(conversation_id, user_id, title, created_at, updated_at,
				 source_label, source_url, saved_title)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			conversationID, req.UserID, req.Title, now, now,
			req.SourceLabel, req.SourceURL, req.Title,
		); err != nil {
			return "", "", fmt.Errorf("chatstore save insert: %w", err)
		}
	case err != nil:
		return "", "", fmt.Errorf("chatstore save read: %w", err)
	case deletedAt.Valid:
		return conversationID, SaveSkippedDeleted, nil
	// A derived id embeds the user, so a foreign owner means this id collided
	// with one allocated elsewhere. Refuse rather than overwrite their chat.
	case owner != req.UserID:
		return conversationID, SaveSkippedConflict, nil
	default:
		status, err = s.resolveExisting(ctx, tx, conversationID, req, now)
		if err != nil {
			return "", "", err
		}
		if status == SaveSkippedDiverged {
			return conversationID, status, nil
		}
	}

	if err := s.insertSaved(ctx, tx, conversationID, req, now); err != nil {
		return "", "", err
	}
	if err := tx.Commit(); err != nil {
		return "", "", fmt.Errorf("chatstore save commit: %w", err)
	}
	return conversationID, status, nil
}

// resolveExisting applies the conflict policy to a copy that already exists and
// reports the status the caller should return. It clears the rows the chosen
// policy discards; the caller inserts the new ones.
func (s *Store) resolveExisting(
	ctx context.Context, tx *sql.Tx, conversationID string, req SaveRequest, now int64,
) (SaveStatus, error) {
	if req.OnConflict == OnConflictAppend {
		return SaveAppended, s.touchSaved(ctx, tx, conversationID, req, now)
	}
	if req.OnConflict == OnConflictSkip {
		diverged, err := hasUserAuthoredRows(ctx, tx, conversationID)
		if err != nil {
			return "", err
		}
		if diverged {
			return SaveSkippedDiverged, nil
		}
	}
	// Interactions share the message seq space, so leaving them behind would
	// interleave stale forms into the refreshed transcript.
	for _, q := range []string{
		`DELETE FROM messages WHERE conversation_id = ?`,
		`DELETE FROM interactions WHERE conversation_id = ?`,
	} {
		if _, err := tx.ExecContext(ctx, q, conversationID); err != nil {
			return "", fmt.Errorf("chatstore save clear: %w", err)
		}
	}
	return SaveReplaced, s.touchSaved(ctx, tx, conversationID, req, now)
}

// touchSaved refreshes the copy's metadata. The title is left alone when the
// user has renamed it, and an empty title never blanks one that exists: a save
// must not undo the user's own edit any more than it may delete their turns.
func (s *Store) touchSaved(
	ctx context.Context, tx *sql.Tx, conversationID string, req SaveRequest, now int64,
) error {
	if _, err := tx.ExecContext(ctx, `
		UPDATE conversations
		SET title = CASE
				WHEN ? = '' THEN title
				WHEN title <> COALESCE(saved_title, '') THEN title
				ELSE ?
			END,
			saved_title = CASE WHEN ? = '' THEN saved_title ELSE ? END,
			updated_at = ?, source_label = ?, source_url = ?
		WHERE conversation_id = ?`,
		req.Title, req.Title, req.Title, req.Title,
		now, req.SourceLabel, req.SourceURL, conversationID,
	); err != nil {
		return fmt.Errorf("chatstore save update: %w", err)
	}
	return nil
}

// hasUserAuthoredRows reports whether the copy holds anything this API did not
// write: a reply the user typed into it, or an interaction they answered.
func hasUserAuthoredRows(ctx context.Context, tx *sql.Tx, conversationID string) (bool, error) {
	var n int
	if err := tx.QueryRowContext(ctx, `
		SELECT
			(SELECT COUNT(*) FROM messages WHERE conversation_id = ? AND origin <> ?) +
			(SELECT COUNT(*) FROM interactions WHERE conversation_id = ?)`,
		conversationID, originSave, conversationID,
	).Scan(&n); err != nil {
		return false, fmt.Errorf("chatstore save divergence check: %w", err)
	}
	return n > 0, nil
}

// insertSaved writes the request's messages after whatever survived the conflict
// policy, keeping the newest when the thread would exceed the per-conversation cap.
func (s *Store) insertSaved(
	ctx context.Context, tx *sql.Tx, conversationID string, req SaveRequest, now int64,
) error {
	seq, err := nextSeqTx(ctx, tx, conversationID)
	if err != nil {
		return err
	}
	var existing int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages WHERE conversation_id = ?`, conversationID,
	).Scan(&existing); err != nil {
		return fmt.Errorf("chatstore save count: %w", err)
	}

	msgs := req.Messages
	if room := MaxMessagesPerConversation - existing; len(msgs) > room {
		if room <= 0 {
			return nil
		}
		msgs = msgs[len(msgs)-room:]
	}

	for _, m := range msgs {
		created := now
		if !m.Timestamp.IsZero() {
			created = m.Timestamp.UnixMilli()
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO messages
				(id, conversation_id, user_id, role, content, seq, created_at, attachments, author, origin)
			VALUES (?, ?, ?, ?, ?, ?, ?, '', ?, ?)`,
			uuid.NewString(), conversationID, req.UserID, m.Role,
			TruncateRunes(m.Content, MaxMessageContentRunes), seq, created, m.Author, originSave,
		); err != nil {
			return fmt.Errorf("chatstore save message: %w", err)
		}
		seq++
	}
	return nil
}
