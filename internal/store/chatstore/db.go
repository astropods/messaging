package chatstore

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
)

type dialect int

const (
	dialectSQLite dialect = iota
	dialectPostgres
)

// rebind rewrites ? placeholders as $1, $2, ... for Postgres, skipping quoted literals.
func (d dialect) rebind(query string) string {
	if d != dialectPostgres || !strings.Contains(query, "?") {
		return query
	}
	var b strings.Builder
	b.Grow(len(query) + 16)
	n, quoted := 0, false
	for i := 0; i < len(query); i++ {
		c := query[i]
		switch {
		case c == '\'':
			quoted = !quoted
		case c == '?' && !quoted:
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// nextSeqExpr takes two conversation id arguments. Each MAX reads one end of a (conversation_id, seq) index.
func (d dialect) nextSeqExpr() string {
	greatest := "MAX"
	if d == dialectPostgres {
		greatest = "GREATEST"
	}
	return greatest + `(
		(SELECT COALESCE(MAX(seq), 0) FROM messages WHERE conversation_id = ?),
		(SELECT COALESCE(MAX(seq), 0) FROM interactions WHERE conversation_id = ?)
	) + 1`
}

type conn struct {
	*sql.DB
	dialect dialect
}

func (c *conn) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return c.DB.ExecContext(ctx, c.dialect.rebind(query), args...)
}

func (c *conn) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return c.DB.QueryContext(ctx, c.dialect.rebind(query), args...)
}

func (c *conn) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return c.DB.QueryRowContext(ctx, c.dialect.rebind(query), args...)
}

func (c *conn) BeginTx(ctx context.Context, opts *sql.TxOptions) (*txn, error) {
	t, err := c.DB.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &txn{Tx: t, dialect: c.dialect}, nil
}

// beginConversation opens a transaction that holds the conversation's write lock until it ends.
func (c *conn) beginConversation(ctx context.Context, conversationID string) (*txn, error) {
	t, err := c.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	if err := t.lockConversation(ctx, conversationID); err != nil {
		_ = t.Rollback()
		return nil, err
	}
	return t, nil
}

type txn struct {
	*sql.Tx
	dialect dialect
}

func (t *txn) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return t.Tx.ExecContext(ctx, t.dialect.rebind(query), args...)
}

func (t *txn) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return t.Tx.QueryContext(ctx, t.dialect.rebind(query), args...)
}

func (t *txn) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return t.Tx.QueryRowContext(ctx, t.dialect.rebind(query), args...)
}

// lockConversation is a no-op on SQLite, whose single connection already serializes writers.
func (t *txn) lockConversation(ctx context.Context, conversationID string) error {
	if t.dialect != dialectPostgres {
		return nil
	}
	if _, err := t.Tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock($1, hashtext($2))`, advisoryLockConversation, conversationID,
	); err != nil {
		return fmt.Errorf("chatstore lock conversation: %w", err)
	}
	return nil
}
