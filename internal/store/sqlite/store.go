// Package sqlite is the deployment-local chat persistence for the messaging
// sidecar. It stores conversation metadata (sidebar: list, title, recency,
// soft-delete) and message bodies in an embedded SQLite database, keyed by the
// opaque WorkOS user id from the OIDC identity header.
//
// This database lives on ephemeral pod storage (emptyDir). It is the hot
// working store; Langfuse traces remain the durable source of truth and are
// used to rebuild this database after a pod is rescheduled (see restore.go).
// No chat content is written to astro-server or its RDS.
package sqlite

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

// MaxMessageContentRunes bounds a single stored message body.
const MaxMessageContentRunes = 128_000

// MaxMessagesPerConversation caps messages per thread.
const MaxMessagesPerConversation = 1000

// ErrMessageLimitReached is returned when an append would exceed the per-thread cap.
var ErrMessageLimitReached = errors.New("conversation message limit reached")

// Conversation is one row of conversation metadata.
type Conversation struct {
	ConversationID     string
	UserID             string
	Title              string
	CreatedAt          time.Time
	UpdatedAt          time.Time
	AssistantStreaming bool // derived: the latest message is from the user (turn in flight)
}

// Message is one chat turn row.
type Message struct {
	ID      string
	Role    string
	Content string
	Seq     int
}

// Store wraps the SQLite database. A single connection serializes writes, which
// is sufficient for the tiny per-pod chat workload and avoids "database is
// locked" under concurrent send/stream persistence.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the SQLite database at path and applies the
// schema. The pragmas enable WAL and a busy timeout for resilience.
func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("chat sqlite open: %w", err)
	}
	// SQLite is a single-writer engine; one connection keeps writes serialized
	// and sidesteps lock contention for this low-volume store.
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("chat sqlite ping: %w", err)
	}
	if err := createSchema(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// Close closes the underlying database.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func createSchema(db *sql.DB) error {
	const schema = `
CREATE TABLE IF NOT EXISTS conversations (
	conversation_id TEXT PRIMARY KEY,
	user_id         TEXT NOT NULL,
	title           TEXT NOT NULL DEFAULT '',
	created_at      INTEGER NOT NULL,
	updated_at      INTEGER NOT NULL,
	deleted_at      INTEGER
);
CREATE INDEX IF NOT EXISTS idx_conversations_user
	ON conversations(user_id, updated_at DESC);

CREATE TABLE IF NOT EXISTS messages (
	id              TEXT PRIMARY KEY,
	conversation_id TEXT NOT NULL,
	user_id         TEXT NOT NULL DEFAULT '',
	role            TEXT NOT NULL,
	content         TEXT NOT NULL,
	seq             INTEGER NOT NULL,
	created_at      INTEGER NOT NULL,
	UNIQUE(conversation_id, seq)
);
CREATE INDEX IF NOT EXISTS idx_messages_conv
	ON messages(conversation_id, seq);
`
	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("chat sqlite schema: %w", err)
	}
	return nil
}

// Upsert creates the conversation row if absent or, when it exists, bumps
// recency and (only when a non-empty title is supplied) renames it. An empty
// title is a pure recency "touch" that never clobbers an existing title. The
// update is scoped to the owning user.
func (s *Store) Upsert(conversationID, userID, title string) error {
	now := time.Now().UnixMilli()
	_, err := s.db.Exec(`
		INSERT INTO conversations (conversation_id, user_id, title, created_at, updated_at)
		VALUES (?, ?, COALESCE(NULLIF(?, ''), ''), ?, ?)
		ON CONFLICT(conversation_id) DO UPDATE SET
			title      = COALESCE(NULLIF(?, ''), conversations.title),
			updated_at = ?,
			deleted_at = NULL
		WHERE conversations.user_id = ?`,
		conversationID, userID, title, now, now,
		title, now, userID,
	)
	if err != nil {
		return fmt.Errorf("chatstore upsert: %w", err)
	}
	return nil
}

// EnsureForSend creates the conversation on first send (using the derived title)
// and otherwise only bumps recency — it never overwrites an existing title.
func (s *Store) EnsureForSend(conversationID, userID, title string) error {
	now := time.Now().UnixMilli()
	_, err := s.db.Exec(`
		INSERT INTO conversations (conversation_id, user_id, title, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(conversation_id) DO UPDATE SET
			updated_at = ?,
			deleted_at = NULL
		WHERE conversations.user_id = ?`,
		conversationID, userID, title, now, now,
		now, userID,
	)
	if err != nil {
		return fmt.Errorf("chatstore ensure: %w", err)
	}
	return nil
}

// Get returns one active conversation, or nil if it does not exist or is deleted.
func (s *Store) Get(conversationID string) (*Conversation, error) {
	row := s.db.QueryRow(`
		SELECT conversation_id, user_id, title, created_at, updated_at
		FROM conversations
		WHERE conversation_id = ? AND deleted_at IS NULL`,
		conversationID,
	)
	var (
		conv             Conversation
		createdMs, updMs int64
	)
	err := row.Scan(&conv.ConversationID, &conv.UserID, &conv.Title, &createdMs, &updMs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("chatstore get: %w", err)
	}
	conv.CreatedAt = time.UnixMilli(createdMs)
	conv.UpdatedAt = time.UnixMilli(updMs)
	return &conv, nil
}

// ListByUser returns the user's active conversations, most-recent first, with
// AssistantStreaming derived from whether the latest message is still the user's.
func (s *Store) ListByUser(userID string) ([]Conversation, error) {
	rows, err := s.db.Query(`
		SELECT c.conversation_id, c.user_id, c.title, c.created_at, c.updated_at,
			COALESCE((
				SELECT m.role FROM messages m
				WHERE m.conversation_id = c.conversation_id
				ORDER BY m.seq DESC LIMIT 1
			), '') AS last_role
		FROM conversations c
		WHERE c.user_id = ? AND c.deleted_at IS NULL
		ORDER BY c.updated_at DESC`,
		userID,
	)
	if err != nil {
		return nil, fmt.Errorf("chatstore list: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	out := make([]Conversation, 0, 16)
	for rows.Next() {
		var (
			conv             Conversation
			createdMs, updMs int64
			lastRole         string
		)
		if err := rows.Scan(&conv.ConversationID, &conv.UserID, &conv.Title, &createdMs, &updMs, &lastRole); err != nil {
			return nil, fmt.Errorf("chatstore list scan: %w", err)
		}
		conv.CreatedAt = time.UnixMilli(createdMs)
		conv.UpdatedAt = time.UnixMilli(updMs)
		conv.AssistantStreaming = lastRole == "user"
		out = append(out, conv)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("chatstore list rows: %w", err)
	}
	return out, nil
}

// SoftDelete marks a conversation deleted for the owning user. Returns true when
// a row was affected. The message bodies are left in place locally and the
// conversation's Langfuse traces are intentionally NOT erased — see the
// conundrum note in HandleDeleteChatConversation.
func (s *Store) SoftDelete(conversationID, userID string) (bool, error) {
	res, err := s.db.Exec(`
		UPDATE conversations SET deleted_at = ?
		WHERE conversation_id = ? AND user_id = ? AND deleted_at IS NULL`,
		time.Now().UnixMilli(), conversationID, userID,
	)
	if err != nil {
		return false, fmt.Errorf("chatstore soft delete: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("chatstore soft delete rows: %w", err)
	}
	return n > 0, nil
}

// ListMessages returns the full ordered thread for one conversation.
func (s *Store) ListMessages(conversationID string) ([]Message, error) {
	rows, err := s.db.Query(`
		SELECT id, role, content, seq
		FROM messages
		WHERE conversation_id = ?
		ORDER BY seq ASC`,
		conversationID,
	)
	if err != nil {
		return nil, fmt.Errorf("chatstore list messages: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	out := make([]Message, 0, 32)
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.Role, &m.Content, &m.Seq); err != nil {
			return nil, fmt.Errorf("chatstore list messages scan: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("chatstore list messages rows: %w", err)
	}
	return out, nil
}

// AppendMessage appends one message to a conversation, assigning the next
// sequence number. Content is truncated to MaxMessageContentRunes.
func (s *Store) AppendMessage(conversationID, userID, role, content string) (Message, error) {
	content = truncateRunes(content, MaxMessageContentRunes)

	var nextSeq int
	if err := s.db.QueryRow(`
		SELECT COALESCE(MAX(seq), 0) + 1 FROM messages WHERE conversation_id = ?`,
		conversationID,
	).Scan(&nextSeq); err != nil {
		return Message{}, fmt.Errorf("chatstore next seq: %w", err)
	}
	if nextSeq > MaxMessagesPerConversation {
		return Message{}, ErrMessageLimitReached
	}

	msg := Message{ID: uuid.NewString(), Role: role, Content: content, Seq: nextSeq}
	if _, err := s.db.Exec(`
		INSERT INTO messages (id, conversation_id, user_id, role, content, seq, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		msg.ID, conversationID, userID, role, content, nextSeq, time.Now().UnixMilli(),
	); err != nil {
		return Message{}, fmt.Errorf("chatstore append message: %w", err)
	}
	return msg, nil
}

// UpsertAssistantProgress mirrors a streaming assistant reply into a single row
// for the current turn: it updates the trailing assistant message in place, or
// appends a new assistant message when the latest row is the user's. Returns the
// assistant message id.
func (s *Store) UpsertAssistantProgress(conversationID, content string) (string, error) {
	content = truncateRunes(content, MaxMessageContentRunes)

	var (
		lastID   string
		lastRole string
	)
	err := s.db.QueryRow(`
		SELECT id, role FROM messages
		WHERE conversation_id = ?
		ORDER BY seq DESC LIMIT 1`,
		conversationID,
	).Scan(&lastID, &lastRole)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("chatstore assistant progress read: %w", err)
	}

	if lastRole == "assistant" {
		if _, err := s.db.Exec(`UPDATE messages SET content = ? WHERE id = ?`, content, lastID); err != nil {
			return "", fmt.Errorf("chatstore assistant progress update: %w", err)
		}
		s.touchConversation(conversationID)
		return lastID, nil
	}

	msg, err := s.AppendMessage(conversationID, "", "assistant", content)
	if err != nil {
		return "", err
	}
	s.touchConversation(conversationID)
	return msg.ID, nil
}

func (s *Store) touchConversation(conversationID string) {
	_, _ = s.db.Exec(`UPDATE conversations SET updated_at = ? WHERE conversation_id = ?`,
		time.Now().UnixMilli(), conversationID)
}

func truncateRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max])
}
