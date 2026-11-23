package chatstore

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// PostgresSchema holds every table messaging owns, so it never touches the agent's tables.
const PostgresSchema = "astro_messaging"

const (
	advisoryLockMigrate      int32 = 0x6d736731
	advisoryLockConversation int32 = 0x6d736732
)

const postgresMaxConns = 5

// sharedReapGrace keeps a sidecar from reaping a turn another sidecar is running.
const sharedReapGrace = 15 * time.Minute

// postgresMigrations run in order, each once; their index plus one is the schema version.
var postgresMigrations = []string{
	`
CREATE TABLE conversations (
	conversation_id TEXT PRIMARY KEY,
	user_id         TEXT NOT NULL,
	title           TEXT NOT NULL DEFAULT '',
	created_at      BIGINT NOT NULL,
	updated_at      BIGINT NOT NULL,
	deleted_at      BIGINT,
	source_label    TEXT NOT NULL DEFAULT '',
	source_url      TEXT NOT NULL DEFAULT '',
	saved_title     TEXT NOT NULL DEFAULT '',
	room_id         TEXT NOT NULL DEFAULT '',
	head_id         TEXT NOT NULL DEFAULT '',
	head_newest_id  TEXT NOT NULL DEFAULT '',
	agent_head_id   TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_conversations_sidebar ON conversations (user_id, room_id, updated_at DESC) WHERE deleted_at IS NULL;

CREATE TABLE messages (
	id              TEXT PRIMARY KEY,
	conversation_id TEXT NOT NULL,
	user_id         TEXT NOT NULL DEFAULT '',
	role            TEXT NOT NULL,
	content         TEXT NOT NULL,
	seq             INTEGER NOT NULL,
	created_at      BIGINT NOT NULL,
	attachments     TEXT NOT NULL DEFAULT '',
	author          TEXT NOT NULL DEFAULT '',
	origin          TEXT NOT NULL DEFAULT '',
	parent_id       TEXT NOT NULL DEFAULT '',
	UNIQUE (conversation_id, seq)
);

CREATE TABLE interactions (
	id              TEXT    NOT NULL,
	conversation_id TEXT    NOT NULL,
	user_id         TEXT    NOT NULL,
	seq             INTEGER NOT NULL,
	render_json     TEXT    NOT NULL,
	status          TEXT    NOT NULL DEFAULT 'pending',
	response_json   TEXT,
	created_at      BIGINT  NOT NULL,
	responded_at    BIGINT,
	PRIMARY KEY (conversation_id, id),
	UNIQUE (conversation_id, seq)
);
CREATE INDEX idx_interactions_pending ON interactions (conversation_id, seq) WHERE status = 'pending';

CREATE TABLE conversation_contexts (
	conversation_id TEXT PRIMARY KEY,
	data            JSONB  NOT NULL,
	updated_at      BIGINT NOT NULL
);
CREATE INDEX idx_conversation_contexts_updated ON conversation_contexts (updated_at);
`,
}

// OpenPostgres connects to the database at url and migrates the astro_messaging schema.
func OpenPostgres(ctx context.Context, url string) (*Store, error) {
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("chat postgres parse url: %w", err)
	}
	cfg.RuntimeParams["search_path"] = PostgresSchema
	db := stdlib.OpenDB(*cfg)
	db.SetMaxOpenConns(postgresMaxConns)
	db.SetMaxIdleConns(postgresMaxConns)
	db.SetConnMaxIdleTime(5 * time.Minute)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("chat postgres ping: %w", err)
	}
	if err := migratePostgres(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: &conn{DB: db, dialect: dialectPostgres}, reapGrace: sharedReapGrace}, nil
}

// migratePostgres holds an advisory lock so sidecars that start together migrate once.
func migratePostgres(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("chat postgres migrate begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, advisoryLockMigrate); err != nil {
		return fmt.Errorf("chat postgres migrate lock: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `CREATE SCHEMA IF NOT EXISTS `+PostgresSchema); err != nil {
		return fmt.Errorf("chat postgres create schema: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS `+PostgresSchema+`.schema_version (
			version    INTEGER PRIMARY KEY,
			applied_at BIGINT NOT NULL
		)`); err != nil {
		return fmt.Errorf("chat postgres create version table: %w", err)
	}
	var current int
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version), 0) FROM `+PostgresSchema+`.schema_version`,
	).Scan(&current); err != nil {
		return fmt.Errorf("chat postgres read version: %w", err)
	}
	if current > len(postgresMigrations) {
		return fmt.Errorf("chat postgres schema version %d is newer than this build supports (%d)", current, len(postgresMigrations))
	}
	for v := current + 1; v <= len(postgresMigrations); v++ {
		if _, err := tx.ExecContext(ctx, `SET LOCAL search_path TO `+PostgresSchema); err != nil {
			return fmt.Errorf("chat postgres set search path: %w", err)
		}
		if _, err := tx.ExecContext(ctx, postgresMigrations[v-1]); err != nil {
			return fmt.Errorf("chat postgres migration %d: %w", v, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO `+PostgresSchema+`.schema_version (version, applied_at) VALUES ($1, $2)`,
			v, time.Now().UnixMilli(),
		); err != nil {
			return fmt.Errorf("chat postgres record migration %d: %w", v, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("chat postgres migrate commit: %w", err)
	}
	return nil
}
