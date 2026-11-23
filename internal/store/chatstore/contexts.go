package chatstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/astropods/messaging/internal/store"
	"github.com/astropods/messaging/pkg/types"
)

// ContextStore is a store.ConversationStore on the shared Postgres chat database.
type ContextStore struct {
	db  *conn
	ttl time.Duration
}

var _ store.ConversationStore = (*ContextStore)(nil)

// Contexts returns the conversation context store, which only Postgres supports.
// A ttl of zero or less keeps contexts forever. Expired contexts are deleted here.
func (s *Store) Contexts(ctx context.Context, ttl time.Duration) (*ContextStore, error) {
	if s.db.dialect != dialectPostgres {
		return nil, errors.New("chatstore contexts: needs the postgres chat store")
	}
	cs := &ContextStore{db: s.db, ttl: ttl}
	if ttl > 0 {
		if _, err := s.db.ExecContext(ctx,
			`DELETE FROM conversation_contexts WHERE updated_at <= ?`, cs.cutoff(),
		); err != nil {
			return nil, fmt.Errorf("chatstore contexts prune: %w", err)
		}
	}
	return cs, nil
}

func (c *ContextStore) cutoff() int64 {
	if c.ttl <= 0 {
		return math.MinInt64
	}
	return time.Now().Add(-c.ttl).UnixMilli()
}

func (c *ContextStore) Get(ctx context.Context, conversationID string) (*types.ConversationContext, error) {
	var data []byte
	err := c.db.QueryRowContext(ctx,
		`SELECT data FROM conversation_contexts WHERE conversation_id = ? AND updated_at > ?`,
		conversationID, c.cutoff(),
	).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("conversation not found: %s", conversationID)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get conversation: %w", err)
	}
	var out types.ConversationContext
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("failed to unmarshal conversation: %w", err)
	}
	return &out, nil
}

// Create replaces a context that has expired but refuses a live one.
func (c *ContextStore) Create(ctx context.Context, cc *types.ConversationContext) error {
	data, err := json.Marshal(cc)
	if err != nil {
		return fmt.Errorf("failed to marshal conversation: %w", err)
	}
	res, err := c.db.ExecContext(ctx, `
		INSERT INTO conversation_contexts (conversation_id, data, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT (conversation_id) DO UPDATE SET data = excluded.data, updated_at = excluded.updated_at
		WHERE conversation_contexts.updated_at <= ?`,
		cc.ConversationID, string(data), time.Now().UnixMilli(), c.cutoff(),
	)
	if err != nil {
		return fmt.Errorf("failed to create conversation: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("failed to create conversation: %w", err)
	} else if n == 0 {
		return fmt.Errorf("conversation already exists: %s", cc.ConversationID)
	}
	return nil
}

// Update writes the context whether or not it exists, as RedisStore does.
func (c *ContextStore) Update(ctx context.Context, cc *types.ConversationContext) error {
	data, err := json.Marshal(cc)
	if err != nil {
		return fmt.Errorf("failed to marshal conversation: %w", err)
	}
	if _, err := c.db.ExecContext(ctx, `
		INSERT INTO conversation_contexts (conversation_id, data, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT (conversation_id) DO UPDATE SET data = excluded.data, updated_at = excluded.updated_at`,
		cc.ConversationID, string(data), time.Now().UnixMilli(),
	); err != nil {
		return fmt.Errorf("failed to update conversation: %w", err)
	}
	return nil
}

func (c *ContextStore) Delete(ctx context.Context, conversationID string) error {
	res, err := c.db.ExecContext(ctx,
		`DELETE FROM conversation_contexts WHERE conversation_id = ?`, conversationID,
	)
	if err != nil {
		return fmt.Errorf("failed to delete conversation: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("failed to delete conversation: %w", err)
	} else if n == 0 {
		return fmt.Errorf("conversation not found: %s", conversationID)
	}
	return nil
}

// Close is a no-op: the chat Store owns the connection pool.
func (c *ContextStore) Close() error { return nil }
