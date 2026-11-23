package chatstore

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/astropods/messaging/pkg/types"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

const testDatabaseURLEnv = "CHAT_TEST_DATABASE_URL"

func requirePostgres(t *testing.T) string {
	t.Helper()
	base := os.Getenv(testDatabaseURLEnv)
	if base == "" {
		t.Skip(testDatabaseURLEnv + " is unset")
	}
	return base
}

// newPostgresTestDatabase creates an empty database for one test and returns its URL.
func newPostgresTestDatabase(t *testing.T) string {
	t.Helper()
	base := requirePostgres(t)
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	name := "chat_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	_, err = admin.Exec("CREATE DATABASE " + name)
	_ = admin.Close()
	if err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		admin, err := sql.Open("pgx", base)
		if err != nil {
			t.Errorf("open admin connection: %v", err)
			return
		}
		defer admin.Close() //nolint:errcheck
		if _, err := admin.Exec("DROP DATABASE " + name + " WITH (FORCE)"); err != nil {
			t.Errorf("drop database %s: %v", name, err)
		}
	})
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse %s: %v", testDatabaseURLEnv, err)
	}
	u.Path = "/" + name
	return u.String()
}

func openPostgresTestStore(t *testing.T, dbURL string) *Store {
	t.Helper()
	st, err := OpenPostgres(t.Context(), dbURL)
	if err != nil {
		t.Fatalf("open postgres store: %v", err)
	}
	// Tests open many stores at once; one idle connection each stays under the server's client limit.
	st.db.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// newSidecarPair opens two stores on one database, each with its own pool, as two sidecars would.
func newSidecarPair(t *testing.T) (*Store, *Store) {
	t.Helper()
	dbURL := newPostgresTestDatabase(t)
	return openPostgresTestStore(t, dbURL), openPostgresTestStore(t, dbURL)
}

func TestTwoSidecarsAppendingToOneConversationLoseNoMessages(t *testing.T) {
	a, b := newSidecarPair(t)
	ctx := t.Context()
	if _, err := a.EnsureForSend(ctx, "conv", "owner", "t"); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}

	const perSidecar = 25
	var wg sync.WaitGroup
	errs := make(chan error, 2*perSidecar)
	for _, st := range []*Store{a, b} {
		for i := range perSidecar {
			wg.Go(func() {
				if _, err := st.AppendMessage(ctx, "conv", "", "assistant", fmt.Sprintf("m%d", i), ""); err != nil {
					errs <- err
				}
			})
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent append failed: %v", err)
	}

	msgs, err := b.ListMessages(ctx, "conv")
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	if len(msgs) != 2*perSidecar {
		t.Fatalf("got %d messages, want %d: concurrent appends were lost", len(msgs), 2*perSidecar)
	}
	for i, m := range msgs {
		if m.Seq != i+1 {
			t.Fatalf("message %d has seq %d, want %d: seqs must be contiguous", i, m.Seq, i+1)
		}
	}
}

func TestTwoSidecarsFinishingOneTurnWriteOneAssistantRow(t *testing.T) {
	a, b := newSidecarPair(t)
	ctx := t.Context()

	for round := range 20 {
		conv := fmt.Sprintf("conv-%d", round)
		if _, err := a.EnsureForSend(ctx, conv, "owner", "t"); err != nil {
			t.Fatalf("seed %s: %v", conv, err)
		}
		if _, err := a.AppendMessage(ctx, conv, "owner", "user", "question", ""); err != nil {
			t.Fatalf("append user turn %s: %v", conv, err)
		}

		var wg sync.WaitGroup
		errs := make(chan error, 3)
		wg.Go(func() {
			if _, err := a.UpsertAssistantProgress(ctx, conv, "partial", ""); err != nil {
				errs <- err
			}
		})
		wg.Go(func() {
			if _, err := b.UpsertAssistantProgress(ctx, conv, "partial reply", ""); err != nil {
				errs <- err
			}
		})
		wg.Go(func() {
			if _, err := b.FinalizeStopped(ctx, conv, "owner", "partial reply, stopped"); err != nil {
				errs <- err
			}
		})
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("%s: concurrent write failed: %v", conv, err)
		}

		msgs, err := a.ListMessages(ctx, conv)
		if err != nil {
			t.Fatalf("list %s: %v", conv, err)
		}
		if len(msgs) != 2 || msgs[0].Role != "user" || msgs[1].Role != "assistant" {
			roles := make([]string, len(msgs))
			for i, m := range msgs {
				roles[i] = m.Role
			}
			t.Fatalf("%s: roles = %v, want [user assistant]: one turn must get one assistant row", conv, roles)
		}
	}
}

func TestSidecarsStartingTogetherMigrateOnce(t *testing.T) {
	dbURL := newPostgresTestDatabase(t)

	var wg sync.WaitGroup
	stores := make([]*Store, 4)
	errs := make([]error, len(stores))
	for i := range stores {
		wg.Go(func() { stores[i], errs[i] = OpenPostgres(t.Context(), dbURL) })
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("sidecar %d failed to open: %v", i, err)
		}
		t.Cleanup(func() { _ = stores[i].Close() })
	}

	var rows, version int
	if err := stores[0].db.QueryRowContext(t.Context(),
		`SELECT COUNT(*), MAX(version) FROM schema_version`).Scan(&rows, &version); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	if rows != len(postgresMigrations) || version != len(postgresMigrations) {
		t.Fatalf("schema_version has %d rows at version %d, want %d of each", rows, version, len(postgresMigrations))
	}
}

func TestOpenPostgresRefusesASchemaNewerThanTheBuild(t *testing.T) {
	dbURL := newPostgresTestDatabase(t)
	st := openPostgresTestStore(t, dbURL)
	if _, err := st.db.ExecContext(t.Context(),
		`INSERT INTO schema_version (version, applied_at) VALUES (?, 0)`, len(postgresMigrations)+1); err != nil {
		t.Fatalf("record future version: %v", err)
	}

	newer, err := OpenPostgres(t.Context(), dbURL)
	if err == nil {
		_ = newer.Close()
		t.Fatal("OpenPostgres succeeded on a newer schema, want an error")
	}
}

func TestPostgresStoreLeavesTheAgentTablesAlone(t *testing.T) {
	dbURL := newPostgresTestDatabase(t)
	agent, err := sql.Open("pgx", dbURL)
	if err != nil {
		t.Fatalf("open agent connection: %v", err)
	}
	t.Cleanup(func() { _ = agent.Close() })
	if _, err := agent.Exec(`CREATE TABLE public.conversations (id INT PRIMARY KEY, note TEXT)`); err != nil {
		t.Fatalf("create agent table: %v", err)
	}
	if _, err := agent.Exec(`INSERT INTO public.conversations VALUES (1, 'agent row')`); err != nil {
		t.Fatalf("seed agent table: %v", err)
	}

	st := openPostgresTestStore(t, dbURL)
	if _, err := st.EnsureForSend(t.Context(), "conv", "owner", "t"); err != nil {
		t.Fatalf("ensure conversation: %v", err)
	}

	var note string
	if err := agent.QueryRow(`SELECT note FROM public.conversations WHERE id = 1`).Scan(&note); err != nil || note != "agent row" {
		t.Fatalf("agent table changed: note=%q err=%v", note, err)
	}
	var owner string
	if err := agent.QueryRow(
		`SELECT user_id FROM ` + PostgresSchema + `.conversations WHERE conversation_id = 'conv'`,
	).Scan(&owner); err != nil || owner != "owner" {
		t.Fatalf("chat row not in %s: owner=%q err=%v", PostgresSchema, owner, err)
	}
}

func TestPostgresReapSkipsTurnsAnotherSidecarMayBeRunning(t *testing.T) {
	st := openPostgresTestStore(t, newPostgresTestDatabase(t))
	ctx := t.Context()
	for _, id := range []string{"fresh", "stale"} {
		if _, err := st.EnsureForSend(ctx, id, "owner", "t"); err != nil {
			t.Fatalf("ensure %s: %v", id, err)
		}
		if _, err := st.AppendMessage(ctx, id, "owner", "user", "hi", ""); err != nil {
			t.Fatalf("append %s: %v", id, err)
		}
	}
	if _, err := st.db.ExecContext(ctx, `UPDATE conversations SET updated_at = ? WHERE conversation_id = 'stale'`,
		time.Now().Add(-sharedReapGrace-time.Minute).UnixMilli()); err != nil {
		t.Fatalf("age stale turn: %v", err)
	}

	n, err := st.ReapDanglingUserTurns(ctx)
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if n != 1 {
		t.Fatalf("reaped %d turns, want 1: only the turn idle past the grace", n)
	}
	for id, wantLen := range map[string]int{"fresh": 1, "stale": 2} {
		msgs, err := st.ListMessages(ctx, id)
		if err != nil {
			t.Fatalf("list %s: %v", id, err)
		}
		if len(msgs) != wantLen {
			t.Errorf("%s has %d messages, want %d", id, len(msgs), wantLen)
		}
	}
}

func TestContextsNeedsPostgres(t *testing.T) {
	st, err := Open(t.TempDir() + "/chat.db")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.Contexts(t.Context(), time.Hour); err == nil {
		t.Fatal("Contexts succeeded on SQLite, want an error")
	}
}

func TestContextStoreIsSharedAcrossSidecars(t *testing.T) {
	a, b := newSidecarPair(t)
	ctx := t.Context()
	ca, err := a.Contexts(ctx, time.Hour)
	if err != nil {
		t.Fatalf("contexts a: %v", err)
	}
	cb, err := b.Contexts(ctx, time.Hour)
	if err != nil {
		t.Fatalf("contexts b: %v", err)
	}

	want := &types.ConversationContext{ConversationID: "c1", Platform: "slack", ChannelID: "C1", MessageCount: 1}
	if err := ca.Create(ctx, want); err != nil {
		t.Fatalf("create on a: %v", err)
	}
	if err := cb.Create(ctx, want); err == nil {
		t.Fatal("create on b succeeded for a live context, want an error")
	}
	got, err := cb.Get(ctx, "c1")
	if err != nil {
		t.Fatalf("get on b: %v", err)
	}
	if got.Platform != "slack" || got.ChannelID != "C1" {
		t.Fatalf("b read %+v, want the context a wrote", got)
	}

	got.MessageCount = 2
	if err := cb.Update(ctx, got); err != nil {
		t.Fatalf("update on b: %v", err)
	}
	if again, err := ca.Get(ctx, "c1"); err != nil || again.MessageCount != 2 {
		t.Fatalf("a read count %v (err %v), want 2", again, err)
	}

	if err := ca.Delete(ctx, "c1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := cb.Get(ctx, "c1"); err == nil {
		t.Fatal("get after delete succeeded, want not found")
	}
	if err := ca.Delete(ctx, "c1"); err == nil {
		t.Fatal("second delete succeeded, want not found")
	}
}

func TestContextStoreTreatsExpiredContextsAsGone(t *testing.T) {
	st := openPostgresTestStore(t, newPostgresTestDatabase(t))
	ctx := t.Context()
	cs, err := st.Contexts(ctx, time.Hour)
	if err != nil {
		t.Fatalf("contexts: %v", err)
	}
	if err := cs.Create(ctx, &types.ConversationContext{ConversationID: "c1", Platform: "web"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.db.ExecContext(ctx, `UPDATE conversation_contexts SET updated_at = 0`); err != nil {
		t.Fatalf("expire: %v", err)
	}

	if _, err := cs.Get(ctx, "c1"); err == nil {
		t.Fatal("get returned an expired context")
	}
	if err := cs.Create(ctx, &types.ConversationContext{ConversationID: "c1", Platform: "slack"}); err != nil {
		t.Fatalf("create over an expired context: %v", err)
	}
	if got, err := cs.Get(ctx, "c1"); err != nil || got.Platform != "slack" {
		t.Fatalf("got %+v (err %v), want the new context", got, err)
	}

	if _, err := st.db.ExecContext(ctx, `UPDATE conversation_contexts SET updated_at = 0`); err != nil {
		t.Fatalf("expire again: %v", err)
	}
	if _, err := st.Contexts(ctx, time.Hour); err != nil {
		t.Fatalf("reopen contexts: %v", err)
	}
	var n int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversation_contexts`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d contexts left after reopening (err %v), want expired ones pruned", n, err)
	}
}

func TestRebind(t *testing.T) {
	cases := []struct {
		name     string
		d        dialect
		in, want string
	}{
		{"sqlite keeps placeholders", dialectSQLite, "a = ? AND b = ?", "a = ? AND b = ?"},
		{"postgres numbers placeholders", dialectPostgres, "a = ? AND b IN (?,?)", "a = $1 AND b IN ($2,$3)"},
		{"postgres skips quoted question marks", dialectPostgres, "a = '?' AND b = ? AND c = 'it''s?'", "a = '?' AND b = $1 AND c = 'it''s?'"},
		{"postgres leaves numbered placeholders", dialectPostgres, "a = $1", "a = $1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.d.rebind(tc.in); got != tc.want {
				t.Fatalf("rebind(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
