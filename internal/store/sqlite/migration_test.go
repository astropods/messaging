package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// Open must migrate an existing volume whose messages table predates the
// attachments column (there is no versioned migration framework — createSchema's
// guarded ALTER is the only backfill). Assert Open succeeds and attachments then
// round-trip through append + read.
func TestOpen_MigratesAttachmentsColumnOnExistingDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chat.db")

	// Seed an OLD-schema DB: messages table WITHOUT the attachments column.
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("seed open: %v", err)
	}
	_, err = raw.Exec(`CREATE TABLE messages (
		id TEXT PRIMARY KEY,
		conversation_id TEXT NOT NULL,
		user_id TEXT NOT NULL DEFAULT '',
		role TEXT NOT NULL,
		content TEXT NOT NULL,
		seq INTEGER NOT NULL,
		created_at INTEGER NOT NULL,
		UNIQUE(conversation_id, seq)
	);`)
	if err != nil {
		t.Fatalf("seed schema: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("seed close: %v", err)
	}

	// Open runs createSchema → ensureColumn, adding the missing column.
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open (migration) failed: %v", err)
	}
	defer func() { _ = st.Close() }()

	ctx := context.Background()
	atts := `[{"key":"k1","name":"a.txt","content_type":"text/plain","size":3}]`
	if _, err := st.AppendMessage(ctx, "c1", "u1", "user", "hi", atts); err != nil {
		t.Fatalf("AppendMessage after migration: %v", err)
	}
	msgs, err := st.ListMessages(ctx, "c1")
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Attachments != atts {
		t.Fatalf("attachments not preserved after migration: %+v", msgs)
	}
}
