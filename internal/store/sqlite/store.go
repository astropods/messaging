// Package sqlite is the deployment-local chat persistence for the messaging
// sidecar. It stores conversation metadata (sidebar: list, title, recency,
// soft-delete) and message bodies in an embedded SQLite database, keyed by the
// opaque WorkOS user id from the OIDC identity header.
//
// The database lives on the agent's shared persistent disk (mounted into the
// sidecar), so it survives pod reschedules and is itself the durable source of
// truth for chat history — there is no Langfuse restore. No chat content is
// written to astro-server or its RDS.
package sqlite

import (
	"context"
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

// maxListedConversations bounds the conversation sidebar query so a user with a
// large history doesn't return an unbounded list on every load.
const maxListedConversations = 200

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
	// Attachments is the JSON-encoded array of file attachments on this turn
	// (user uploads or agent-produced files). Empty string means none.
	Attachments string
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
	attachments     TEXT NOT NULL DEFAULT '',
	UNIQUE(conversation_id, seq)
);
CREATE INDEX IF NOT EXISTS idx_messages_conv
	ON messages(conversation_id, seq);

CREATE TABLE IF NOT EXISTS interactions (
	id              TEXT    NOT NULL,
	conversation_id TEXT    NOT NULL,
	user_id         TEXT    NOT NULL,
	seq             INTEGER NOT NULL,
	render_json     TEXT    NOT NULL,
	status          TEXT    NOT NULL DEFAULT 'pending',
	response_json   TEXT,
	created_at      INTEGER NOT NULL,
	responded_at    INTEGER,
	PRIMARY KEY (conversation_id, id),
	UNIQUE (conversation_id, seq)
);
CREATE INDEX IF NOT EXISTS idx_interactions_pending
	ON interactions(conversation_id, seq) WHERE status = 'pending';
`
	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("chat sqlite schema: %w", err)
	}
	// Migrate existing volumes: the messages.attachments column was added after
	// the initial schema shipped. There is no versioned migration framework, and
	// SQLite has no ADD COLUMN IF NOT EXISTS, so add it only when absent.
	if err := ensureColumn(db, "messages", "attachments", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	return nil
}

// ensureColumn adds a column to a table if it is not already present. Used for
// additive migrations on volumes provisioned before the column existed.
func ensureColumn(db *sql.DB, table, column, definition string) error {
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return fmt.Errorf("chat sqlite inspect %s: %w", table, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			cid, notnull, pk int
			name, ctype      string
			dfltValue        sql.NullString
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dfltValue, &pk); err != nil {
			return fmt.Errorf("chat sqlite inspect %s: %w", table, err)
		}
		if name == column {
			return nil // already present
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("chat sqlite inspect %s: %w", table, err)
	}
	if _, err := db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, definition)); err != nil {
		return fmt.Errorf("chat sqlite add column %s.%s: %w", table, column, err)
	}
	return nil
}

// Upsert creates the conversation row if absent or, when it exists, bumps
// recency and (only when a non-empty title is supplied) renames it. An empty
// title is a pure recency "touch" that never clobbers an existing title. The
// update is scoped to the owning user.
func (s *Store) Upsert(ctx context.Context, conversationID, userID, title string) error {
	now := time.Now().UnixMilli()
	_, err := s.db.ExecContext(ctx, `
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

// SetTitle renames an existing conversation owned by the user. Unlike Upsert it
// never creates a row: the UPDATE is scoped to an active, owned conversation, so
// a missing/foreign/deleted conversation affects no rows and returns false. This
// keeps the title-rename endpoint from being able to create or otherwise mutate
// conversations.
func (s *Store) SetTitle(ctx context.Context, conversationID, userID, title string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE conversations SET title = ?, updated_at = ?
		WHERE conversation_id = ? AND user_id = ? AND deleted_at IS NULL`,
		title, time.Now().UnixMilli(), conversationID, userID,
	)
	if err != nil {
		return false, fmt.Errorf("chatstore set title: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("chatstore set title rows: %w", err)
	}
	return n > 0, nil
}

// EnsureForSend creates the conversation on first send (using the derived title)
// and otherwise only bumps recency. It fills in the derived title when the row
// exists but is still untitled — the create-then-send flow (HandleCreateConversation)
// pre-creates the row with an empty title, so titling only on insert would leave
// those threads permanently blank in the sidebar. An already-set title is never
// overwritten.
//
// It returns whether the conversation is owned by userID: a brand-new one is
// (just created here), an existing owned one is, and one owned by another user
// is NOT — the upsert's `WHERE user_id = ?` no-ops on it. Callers use this to
// reject a send into a foreign conversation (a stored cross-user write).
//
// A soft-deleted conversation is never revived: the conflict update is scoped to
// active rows, so a send to a deleted id is a no-op and reports not-owned (404),
// keeping deletes terminal.
func (s *Store) EnsureForSend(ctx context.Context, conversationID, userID, title string) (bool, error) {
	now := time.Now().UnixMilli()
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO conversations (conversation_id, user_id, title, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(conversation_id) DO UPDATE SET
			title      = CASE WHEN conversations.title = '' THEN excluded.title ELSE conversations.title END,
			updated_at = ?
		WHERE conversations.user_id = ? AND conversations.deleted_at IS NULL`,
		conversationID, userID, title, now, now,
		now, userID,
	); err != nil {
		return false, fmt.Errorf("chatstore ensure: %w", err)
	}

	var owner string
	err := s.db.QueryRowContext(ctx,
		`SELECT user_id FROM conversations WHERE conversation_id = ? AND deleted_at IS NULL`,
		conversationID,
	).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("chatstore ensure owner check: %w", err)
	}
	return owner == userID, nil
}

// Get returns one active conversation, or nil if it does not exist or is deleted.
func (s *Store) Get(ctx context.Context, conversationID string) (*Conversation, error) {
	row := s.db.QueryRowContext(ctx, `
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
// Conversations with no messages (e.g. a pre-created "new chat" that was never
// sent to) are excluded, and the result is capped at maxListedConversations —
// both served by the (user_id, updated_at DESC) index.
func (s *Store) ListByUser(ctx context.Context, userID string) ([]Conversation, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.conversation_id, c.user_id, c.title, c.created_at, c.updated_at,
			COALESCE((
				SELECT m.role FROM messages m
				WHERE m.conversation_id = c.conversation_id
				ORDER BY m.seq DESC LIMIT 1
			), '') AS last_role
		FROM conversations c
		WHERE c.user_id = ? AND c.deleted_at IS NULL
			AND EXISTS (SELECT 1 FROM messages m WHERE m.conversation_id = c.conversation_id)
		ORDER BY c.updated_at DESC
		LIMIT ?`,
		userID, maxListedConversations,
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

// SoftDelete marks a conversation deleted for its owner and, in the same
// transaction, cancels its pending interactions so a suspended agent doesn't hang
// on an interaction the user can no longer answer. It returns the ids of the
// interactions it cancelled (for the caller to deliver a CANCEL response to the
// agent) and whether the conversation was deleted.
//
// The message bodies are left in place locally and the conversation's Langfuse
// traces are intentionally NOT erased — see the conundrum note in
// HandleDeleteChatConversation.
func (s *Store) SoftDelete(ctx context.Context, conversationID, userID string) (cancelled []string, deleted bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, fmt.Errorf("chatstore soft delete begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful commit

	res, err := tx.ExecContext(ctx, `
		UPDATE conversations SET deleted_at = ?
		WHERE conversation_id = ? AND user_id = ? AND deleted_at IS NULL`,
		time.Now().UnixMilli(), conversationID, userID,
	)
	if err != nil {
		return nil, false, fmt.Errorf("chatstore soft delete: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, false, fmt.Errorf("chatstore soft delete rows: %w", err)
	}
	if n == 0 {
		return nil, false, nil
	}

	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM interactions WHERE conversation_id = ? AND status = 'pending'`,
		conversationID,
	)
	if err != nil {
		return nil, false, fmt.Errorf("chatstore soft delete pending: %w", err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, false, fmt.Errorf("chatstore soft delete pending scan: %w", err)
		}
		cancelled = append(cancelled, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, false, fmt.Errorf("chatstore soft delete pending rows: %w", err)
	}
	_ = rows.Close()

	if len(cancelled) > 0 {
		if _, err := tx.ExecContext(ctx, `
			UPDATE interactions SET status = 'cancelled', responded_at = ?
			WHERE conversation_id = ? AND status = 'pending'`,
			time.Now().UnixMilli(), conversationID,
		); err != nil {
			return nil, false, fmt.Errorf("chatstore soft delete cancel interactions: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("chatstore soft delete commit: %w", err)
	}
	return cancelled, true, nil
}

// ListMessages returns the full ordered thread for one conversation.
func (s *Store) ListMessages(ctx context.Context, conversationID string) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, role, content, seq, attachments
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
		if err := rows.Scan(&m.ID, &m.Role, &m.Content, &m.Seq, &m.Attachments); err != nil {
			return nil, fmt.Errorf("chatstore list messages scan: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("chatstore list messages rows: %w", err)
	}
	return out, nil
}

// hasMessagesBefore reports whether any message row precedes seq. seq is shared
// with interactions so message rows aren't contiguous — position can't be read
// off seq, hence the EXISTS.
func (s *Store) hasMessagesBefore(ctx context.Context, conversationID string, seq int) (bool, error) {
	var exists bool
	if err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM messages WHERE conversation_id = ? AND seq < ?)`,
		conversationID, seq,
	).Scan(&exists); err != nil {
		return false, fmt.Errorf("chatstore has messages before: %w", err)
	}
	return exists, nil
}

// PageMessages returns one page of a conversation's messages, ordered by seq
// ascending, doing the windowing in SQL so a large thread isn't fully
// materialized to serve a single page. seq is shared with interactions, so
// message rows can have gaps; hasMore is decided by an EXISTS on older message
// rows rather than derived from the oldest seq. When beforeSeq > 0 the page
// immediately preceding that seq is returned; otherwise the newest page. lastRole
// is the role of the newest message in the whole thread (independent of the
// returned page) for the assistant_streaming heuristic; it is "" for an empty thread.
func (s *Store) PageMessages(ctx context.Context, conversationID string, limit, beforeSeq int) (msgs []Message, hasMore bool, oldestSeq int, lastRole string, err error) {
	if limit <= 0 {
		limit = 1
	}
	// Fetch the newest `limit` rows (optionally strictly older than beforeSeq)
	// descending, then reverse to ascending for the caller.
	query := `SELECT id, role, content, seq, attachments FROM messages WHERE conversation_id = ?`
	args := []any{conversationID}
	if beforeSeq > 0 {
		query += ` AND seq < ?`
		args = append(args, beforeSeq)
	}
	query += ` ORDER BY seq DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, 0, "", fmt.Errorf("chatstore page messages: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.Role, &m.Content, &m.Seq, &m.Attachments); err != nil {
			return nil, false, 0, "", fmt.Errorf("chatstore page messages scan: %w", err)
		}
		msgs = append(msgs, m)
	}
	if err := rows.Err(); err != nil {
		return nil, false, 0, "", fmt.Errorf("chatstore page messages rows: %w", err)
	}
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}
	if len(msgs) > 0 {
		oldestSeq = msgs[0].Seq
		hasMore, err = s.hasMessagesBefore(ctx, conversationID, oldestSeq)
		if err != nil {
			return nil, false, 0, "", err
		}
	}

	// The newest message's role drives assistant_streaming and is independent of
	// the returned page (which may be an older window).
	err = s.db.QueryRowContext(ctx,
		`SELECT role FROM messages WHERE conversation_id = ? ORDER BY seq DESC LIMIT 1`,
		conversationID,
	).Scan(&lastRole)
	if errors.Is(err, sql.ErrNoRows) {
		return msgs, hasMore, oldestSeq, "", nil
	}
	if err != nil {
		return nil, false, 0, "", fmt.Errorf("chatstore page last role: %w", err)
	}
	return msgs, hasMore, oldestSeq, lastRole, nil
}

// AppendMessage appends one message to a conversation, assigning the next
// sequence number. Content is truncated to MaxMessageContentRunes.
func (s *Store) AppendMessage(ctx context.Context, conversationID, userID, role, content, attachmentsJSON string) (Message, error) {
	// Assign seq and insert atomically in one transaction. A bare
	// SELECT MAX(seq)+1 followed by a separate INSERT is not atomic even with
	// MaxOpenConns(1) — another writer can grab the connection between the two
	// statements, read the same nextSeq, and one INSERT then loses to the
	// UNIQUE(conversation_id, seq) constraint (a dropped message). The tx holds
	// the single connection across both statements, serialising concurrent
	// appends to the same conversation.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Message{}, fmt.Errorf("chatstore append begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful commit

	msg, err := appendMessageTx(ctx, tx, conversationID, userID, role, content, attachmentsJSON)
	if err != nil {
		return Message{}, err
	}
	if err := tx.Commit(); err != nil {
		return Message{}, fmt.Errorf("chatstore append commit: %w", err)
	}
	return msg, nil
}

// nextSeqTx returns the next sequence number for a conversation, allocated across
// BOTH messages and interactions so the two share one monotonic ordering space
// and interleave by seq. The caller holds the tx (and, with MaxOpenConns(1), the
// only connection), so the read is atomic against concurrent writers.
func nextSeqTx(ctx context.Context, tx *sql.Tx, conversationID string) (int, error) {
	var nextSeq int
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(seq), 0) + 1 FROM (
			SELECT seq FROM messages WHERE conversation_id = ?
			UNION ALL
			SELECT seq FROM interactions WHERE conversation_id = ?
		)`,
		conversationID, conversationID,
	).Scan(&nextSeq); err != nil {
		return 0, fmt.Errorf("chatstore next seq: %w", err)
	}
	return nextSeq, nil
}

// appendMessageTx assigns the next seq and inserts a message within an existing
// transaction. The caller holds the tx (and, with MaxOpenConns(1), the only
// connection), so the read-then-insert is atomic against concurrent writers —
// this is what lets UpsertAssistantProgress and FinalizeStopped fold their
// role-check and append into one tx and not race each other into two rows.
func appendMessageTx(ctx context.Context, tx *sql.Tx, conversationID, userID, role, content, attachmentsJSON string) (Message, error) {
	content = TruncateRunes(content, MaxMessageContentRunes)

	nextSeq, err := nextSeqTx(ctx, tx, conversationID)
	if err != nil {
		return Message{}, err
	}
	// Cap on the message count, not the shared seq: interactions share the seq
	// space for ordering but must not consume the message budget. A user turn is
	// admitted only while the following assistant reply still fits, so the final
	// slot is reserved for that reply: otherwise a user turn could land in the last
	// slot, its reply would exceed the cap and go unpersisted, and the thread would
	// derive assistant_streaming=true forever. Assistant/finalize appends may use
	// the full cap.
	var msgCount int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages WHERE conversation_id = ?`, conversationID,
	).Scan(&msgCount); err != nil {
		return Message{}, fmt.Errorf("chatstore message count: %w", err)
	}
	limit := MaxMessagesPerConversation
	if role == "user" {
		limit = MaxMessagesPerConversation - 1
	}
	if msgCount+1 > limit {
		return Message{}, ErrMessageLimitReached
	}

	msg := Message{ID: uuid.NewString(), Role: role, Content: content, Seq: nextSeq, Attachments: attachmentsJSON}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO messages (id, conversation_id, user_id, role, content, seq, created_at, attachments)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		msg.ID, conversationID, userID, role, content, nextSeq, time.Now().UnixMilli(), attachmentsJSON,
	); err != nil {
		return Message{}, fmt.Errorf("chatstore append message: %w", err)
	}
	return msg, nil
}

// UpsertAssistantProgress mirrors a streaming assistant reply into a single row
// for the current turn: it updates the trailing assistant message in place, or
// appends a new assistant message when the latest row is the user's. Returns the
// assistant message id.
//
// The conversation-exists check, the last-message read, and the update/append
// all run in one transaction. With MaxOpenConns(1) the tx holds the only
// connection, so this can't interleave with FinalizeStopped's own tx — that is
// what prevents a concurrent stop and streamed chunk from both reading
// lastRole=="user" and each appending a duplicate assistant row.
func (s *Store) UpsertAssistantProgress(ctx context.Context, conversationID, content, attachmentsJSON string) (string, error) {
	content = TruncateRunes(content, MaxMessageContentRunes)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("chatstore assistant progress begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful commit

	// Never create message rows for a conversation that doesn't exist in the
	// store. Real user conversations are created on send (EnsureForSend) before
	// any assistant turn is persisted, so this only filters internal agent
	// stream traffic (e.g. the SDK handshake "agent-registration"), which would
	// otherwise leave orphan message rows with no owning conversation.
	var one int
	err = tx.QueryRowContext(ctx,
		`SELECT 1 FROM conversations WHERE conversation_id = ? AND deleted_at IS NULL`,
		conversationID,
	).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("chatstore assistant progress conv check: %w", err)
	}

	var (
		lastID   string
		lastRole string
	)
	err = tx.QueryRowContext(ctx, `
		SELECT id, role FROM messages
		WHERE conversation_id = ?
		ORDER BY seq DESC LIMIT 1`,
		conversationID,
	).Scan(&lastID, &lastRole)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("chatstore assistant progress read: %w", err)
	}

	id := lastID
	if lastRole == "assistant" {
		// Attachments arrive on the END chunk; a mid-stream text update carries
		// none, and must not clobber attachments already written on a prior call.
		if attachmentsJSON != "" {
			if _, err := tx.ExecContext(ctx, `UPDATE messages SET content = ?, attachments = ? WHERE id = ?`, content, attachmentsJSON, lastID); err != nil {
				return "", fmt.Errorf("chatstore assistant progress update: %w", err)
			}
		} else if _, err := tx.ExecContext(ctx, `UPDATE messages SET content = ? WHERE id = ?`, content, lastID); err != nil {
			return "", fmt.Errorf("chatstore assistant progress update: %w", err)
		}
	} else {
		msg, appendErr := appendMessageTx(ctx, tx, conversationID, "", "assistant", content, attachmentsJSON)
		if appendErr != nil {
			return "", appendErr
		}
		id = msg.ID
	}
	if err := touchConversationTx(ctx, tx, conversationID); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("chatstore assistant progress commit: %w", err)
	}
	return id, nil
}

// AppendAssistantMessage always inserts a new assistant row (unlike
// UpsertAssistantProgress, which updates the trailing one), so it won't clobber a
// reply streamed earlier in the turn. No-ops for a missing conversation.
func (s *Store) AppendAssistantMessage(ctx context.Context, conversationID, content string) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("chatstore append assistant begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful commit

	var one int
	err = tx.QueryRowContext(ctx,
		`SELECT 1 FROM conversations WHERE conversation_id = ? AND deleted_at IS NULL`,
		conversationID,
	).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("chatstore append assistant conv check: %w", err)
	}

	msg, err := appendMessageTx(ctx, tx, conversationID, "", "assistant", content, "")
	if err != nil {
		return "", err
	}
	if err := touchConversationTx(ctx, tx, conversationID); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("chatstore append assistant commit: %w", err)
	}
	return msg.ID, nil
}

// FinalizeStopped makes an interrupted turn terminal for a conversation owned by
// userID, using `partial` — the full text the client saw at stop time. If no
// assistant row exists yet (latest message is the user's), it appends one so
// AssistantStreaming (derived as "latest message is the user's") resolves to
// false. If an assistant row already exists (progressively persisted), it grows
// that row to `partial` when `partial` is longer — the progressive snapshot lags
// the buffer by up to one throttle interval, so this recovers the tail the user
// saw. It never shrinks a reply. A missing, deleted, or foreign conversation is a
// no-op. Returns true when a row was written.
//
// Ownership + role-check + write run in one transaction, both to scope the write
// to the caller and to serialize against UpsertAssistantProgress so a concurrent
// stop can't produce a second assistant row.
func (s *Store) FinalizeStopped(ctx context.Context, conversationID, userID, partial string) (bool, error) {
	partial = TruncateRunes(partial, MaxMessageContentRunes)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("chatstore finalize begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful commit

	// The latest message (id/role/content) of the conversation, but only when it
	// exists, is owned by userID, and is not deleted. NULLs (no messages) coalesce
	// to empty strings.
	var lastID, lastRole, lastContent string
	err = tx.QueryRowContext(ctx, `
		SELECT COALESCE(m.id, ''), COALESCE(m.role, ''), COALESCE(m.content, '')
		FROM conversations c
		LEFT JOIN messages m ON m.id = (
			SELECT id FROM messages
			WHERE conversation_id = c.conversation_id
			ORDER BY seq DESC LIMIT 1
		)
		WHERE c.conversation_id = ? AND c.user_id = ? AND c.deleted_at IS NULL`,
		conversationID, userID,
	).Scan(&lastID, &lastRole, &lastContent)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("chatstore finalize stopped read: %w", err)
	}

	switch {
	case lastRole == "user":
		if _, err := appendMessageTx(ctx, tx, conversationID, "", "assistant", partial, ""); err != nil {
			return false, err
		}
	case lastRole == "assistant" && len(partial) > len(lastContent):
		// Grow the progressively-persisted row to the fuller partial (never shrink).
		if _, err := tx.ExecContext(ctx, `UPDATE messages SET content = ? WHERE id = ?`, partial, lastID); err != nil {
			return false, fmt.Errorf("chatstore finalize grow: %w", err)
		}
	default:
		// No messages, or the persisted reply already covers the partial.
		return false, nil
	}

	if err := touchConversationTx(ctx, tx, conversationID); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("chatstore finalize commit: %w", err)
	}
	return true, nil
}

// FinalizeTerminal makes a turn terminal from the agent-response path (an errored
// or otherwise abnormally-ended turn that never wrote an assistant row). Like
// FinalizeStopped it appends `partial` as an assistant row ONLY when the latest
// message is still the user's, so it never shrinks or blanks a completed /
// progressively-persisted reply — important because a spurious error can arrive
// after a turn already reached END (its buffer cleared, so partial is empty).
// Unlike FinalizeStopped it is not ownership-scoped: it runs on the internal
// stream handler, not a user request. No-op for a missing/deleted conversation.
func (s *Store) FinalizeTerminal(ctx context.Context, conversationID, partial string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("chatstore finalize terminal begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful commit

	var lastRole string
	err = tx.QueryRowContext(ctx, `
		SELECT COALESCE((
			SELECT role FROM messages m
			WHERE m.conversation_id = c.conversation_id
			ORDER BY m.seq DESC LIMIT 1
		), '')
		FROM conversations c
		WHERE c.conversation_id = ? AND c.deleted_at IS NULL`,
		conversationID,
	).Scan(&lastRole)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("chatstore finalize terminal read: %w", err)
	}
	if lastRole != "user" {
		return false, nil
	}
	if _, err := appendMessageTx(ctx, tx, conversationID, "", "assistant", partial, ""); err != nil {
		return false, err
	}
	if err := touchConversationTx(ctx, tx, conversationID); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("chatstore finalize terminal commit: %w", err)
	}
	return true, nil
}

// ReapDanglingUserTurns finalizes active conversations whose latest message is
// still the user's. It is meant to run once on startup, when no turn can be in
// flight and the in-memory turn tracker is empty — so a user-last conversation is
// definitionally a turn that was interrupted before any assistant row was
// persisted (agent crash, or a reschedule between the user append and the first
// progressive write). Appending a terminal (empty) assistant row flips it out of
// the derived assistant_streaming state. Returns the number finalized.
func (s *Store) ReapDanglingUserTurns(ctx context.Context) (int, error) {
	// Collect candidate ids first, then finalize. The rows cursor must be drained
	// and closed before FinalizeTerminal's BeginTx: with MaxOpenConns(1) an open
	// cursor holds the only connection and the transaction would otherwise block.
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.conversation_id
		FROM conversations c
		WHERE c.deleted_at IS NULL
			AND (
				SELECT role FROM messages m
				WHERE m.conversation_id = c.conversation_id
				ORDER BY m.seq DESC LIMIT 1
			) = 'user'`)
	if err != nil {
		return 0, fmt.Errorf("chatstore reap query: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("chatstore reap scan: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, fmt.Errorf("chatstore reap rows: %w", err)
	}
	_ = rows.Close()

	finalized := 0
	for _, id := range ids {
		ok, err := s.FinalizeTerminal(ctx, id, "")
		if err != nil {
			return finalized, fmt.Errorf("chatstore reap finalize %q: %w", id, err)
		}
		if ok {
			finalized++
		}
	}
	return finalized, nil
}

func touchConversationTx(ctx context.Context, tx *sql.Tx, conversationID string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE conversations SET updated_at = ? WHERE conversation_id = ?`,
		time.Now().UnixMilli(), conversationID); err != nil {
		return fmt.Errorf("chatstore touch conversation: %w", err)
	}
	return nil
}

// TruncateRunes returns s capped to max runes (never splitting a multi-byte
// rune). Exported so the web adapter can enforce its own caps (titles, message
// bodies) with the same logic the store applies.
func TruncateRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max])
}
