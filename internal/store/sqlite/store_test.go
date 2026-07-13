package sqlite

import (
	"context"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "chat.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// SetTitle renames only an existing, owned, non-deleted conversation. It must
// never create a row (that is the create/first-send path) and must not touch a
// foreign or deleted conversation.
func TestSetTitle(t *testing.T) {
	st := newTestStore(t)

	if err := st.Upsert(t.Context(), "conv-1", "user-1", "original"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	t.Run("renames owned conversation", func(t *testing.T) {
		ok, err := st.SetTitle(t.Context(), "conv-1", "user-1", "renamed")
		if err != nil {
			t.Fatalf("SetTitle: %v", err)
		}
		if !ok {
			t.Fatal("expected rename to apply")
		}
		conv, _ := st.Get(t.Context(), "conv-1")
		if conv == nil || conv.Title != "renamed" {
			t.Fatalf("title = %+v, want renamed", conv)
		}
	})

	t.Run("foreign user does not match", func(t *testing.T) {
		ok, err := st.SetTitle(t.Context(), "conv-1", "user-2", "hijacked")
		if err != nil {
			t.Fatalf("SetTitle: %v", err)
		}
		if ok {
			t.Fatal("expected no rows affected for foreign user")
		}
		conv, _ := st.Get(t.Context(), "conv-1")
		if conv.Title != "renamed" {
			t.Fatalf("foreign rename mutated title: %q", conv.Title)
		}
	})

	t.Run("missing conversation is not created", func(t *testing.T) {
		ok, err := st.SetTitle(t.Context(), "conv-missing", "user-1", "new")
		if err != nil {
			t.Fatalf("SetTitle: %v", err)
		}
		if ok {
			t.Fatal("expected no rows affected for missing conversation")
		}
		if conv, _ := st.Get(t.Context(), "conv-missing"); conv != nil {
			t.Fatalf("SetTitle created a conversation: %+v", conv)
		}
	})

	t.Run("deleted conversation does not match", func(t *testing.T) {
		if err := st.Upsert(t.Context(), "conv-del", "user-1", "orig"); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if _, _, err := st.SoftDelete(t.Context(), "conv-del", "user-1"); err != nil {
			t.Fatalf("soft delete: %v", err)
		}
		ok, err := st.SetTitle(t.Context(), "conv-del", "user-1", "revived")
		if err != nil {
			t.Fatalf("SetTitle: %v", err)
		}
		if ok {
			t.Fatal("expected no rows affected for deleted conversation")
		}
	})
}

// AppendMessage must assign a unique, contiguous seq per message even when many
// writers append to the same conversation concurrently. A non-atomic
// MAX(seq)+1-then-INSERT would let two writers read the same seq and one INSERT
// would then lose to UNIQUE(conversation_id, seq), silently dropping a message.
func TestAppendMessageConcurrentSeqNoDrops(t *testing.T) {
	st := newTestStore(t)
	if _, err := st.EnsureForSend(t.Context(), "conv-1", "user-1", "seed"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	const n = 50
	ctx := t.Context()
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := st.AppendMessage(ctx, "conv-1", "user-1", "user", "m"); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("append failed (message dropped under concurrency): %v", err)
	}

	msgs, err := st.ListMessages(t.Context(), "conv-1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(msgs) != n {
		t.Fatalf("got %d messages, want %d (messages dropped)", len(msgs), n)
	}
	seen := make(map[int]bool, n)
	for _, m := range msgs {
		if m.Seq < 1 || m.Seq > n {
			t.Fatalf("seq %d out of range 1..%d", m.Seq, n)
		}
		if seen[m.Seq] {
			t.Fatalf("duplicate seq %d", m.Seq)
		}
		seen[m.Seq] = true
	}
}

// Concurrent progressive-persist (streaming goroutine) and stop-finalize (cancel
// goroutine) must yield exactly one assistant row, not two. Both fold their
// role-check and write into one transaction; with MaxOpenConns(1) that
// serializes them, so whichever runs first appends the assistant row and the
// other sees lastRole=="assistant" (update-in-place or no-op).
func TestConcurrentStopAndProgressSingleAssistantRow(t *testing.T) {
	ctx := context.Background()
	for i := 0; i < 40; i++ {
		st := newTestStore(t)
		conv := "conv-" + strconv.Itoa(i)
		if _, err := st.EnsureForSend(ctx, conv, "user-1", "t"); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if _, err := st.AppendMessage(ctx, conv, "user-1", "user", "q"); err != nil {
			t.Fatalf("seed user: %v", err)
		}

		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _, _ = st.UpsertAssistantProgress(ctx, conv, "streamed") }()
		go func() { defer wg.Done(); _, _ = st.FinalizeStopped(ctx, conv, "user-1", "partial") }()
		wg.Wait()

		msgs, err := st.ListMessages(ctx, conv)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		var assistants int
		for _, m := range msgs {
			if m.Role == "assistant" {
				assistants++
			}
		}
		if assistants != 1 {
			t.Fatalf("iter %d: got %d assistant rows, want exactly 1: %+v", i, assistants, msgs)
		}
	}
}

// EnsureForSend fills the derived title when a conversation was pre-created with
// an empty title (the create-then-send flow) but never overwrites a title that
// is already set, and titles a brand-new conversation on create.
func TestEnsureForSendFillsEmptyTitleOnce(t *testing.T) {
	st := newTestStore(t)

	// Simulate HandleCreateConversation: pre-create with an empty title.
	if err := st.Upsert(t.Context(), "conv-1", "user-1", ""); err != nil {
		t.Fatalf("pre-create: %v", err)
	}
	if conv, _ := st.Get(t.Context(), "conv-1"); conv == nil || conv.Title != "" {
		t.Fatalf("precondition: want empty title, got %+v", conv)
	}

	// First send fills the derived title and reports ownership.
	if owned, err := st.EnsureForSend(t.Context(), "conv-1", "user-1", "Derived"); err != nil || !owned {
		t.Fatalf("ensure: owned=%v err=%v", owned, err)
	}
	if conv, _ := st.Get(t.Context(), "conv-1"); conv == nil || conv.Title != "Derived" {
		t.Fatalf("title not filled on first send: %+v", conv)
	}

	// Later sends must not overwrite an existing title.
	if owned, err := st.EnsureForSend(t.Context(), "conv-1", "user-1", "Later"); err != nil || !owned {
		t.Fatalf("ensure 2: owned=%v err=%v", owned, err)
	}
	if conv, _ := st.Get(t.Context(), "conv-1"); conv == nil || conv.Title != "Derived" {
		t.Fatalf("existing title overwritten on later send: %+v", conv)
	}

	// A brand-new conversation is created already titled and owned.
	if owned, err := st.EnsureForSend(t.Context(), "conv-2", "user-1", "Fresh"); err != nil || !owned {
		t.Fatalf("ensure new: owned=%v err=%v", owned, err)
	}
	if conv, _ := st.Get(t.Context(), "conv-2"); conv == nil || conv.Title != "Fresh" {
		t.Fatalf("new conversation not titled on create: %+v", conv)
	}
}

// EnsureForSend reports NOT-owned for a conversation owned by another user, and
// leaves it untouched — the guard that stops a caller injecting a send into a
// foreign thread.
func TestEnsureForSendForeignConversationNotOwned(t *testing.T) {
	st := newTestStore(t)
	if _, err := st.EnsureForSend(t.Context(), "conv-1", "owner", "Owner title"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	owned, err := st.EnsureForSend(t.Context(), "conv-1", "attacker", "Hijack")
	if err != nil {
		t.Fatalf("ensure foreign: %v", err)
	}
	if owned {
		t.Fatal("a conversation owned by another user must report not-owned")
	}
	// The owner's conversation must be untouched (still owned, title intact).
	conv, _ := st.Get(t.Context(), "conv-1")
	if conv == nil || conv.UserID != "owner" || conv.Title != "Owner title" {
		t.Fatalf("foreign EnsureForSend mutated the conversation: %+v", conv)
	}
}

// FinalizeStopped only writes to a conversation owned by the caller, and folds
// its role-check + append into one transaction so it can't duplicate an
// assistant row.
func TestFinalizeStoppedOwnership(t *testing.T) {
	st := newTestStore(t)
	if _, err := st.EnsureForSend(t.Context(), "conv-1", "owner", "t"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := st.AppendMessage(t.Context(), "conv-1", "owner", "user", "hi"); err != nil {
		t.Fatalf("seed user msg: %v", err)
	}

	// Foreign caller: no-op, no assistant row appended.
	if ok, err := st.FinalizeStopped(t.Context(), "conv-1", "attacker", "stolen partial"); err != nil || ok {
		t.Fatalf("foreign finalize: ok=%v err=%v (must be no-op)", ok, err)
	}
	msgs, _ := st.ListMessages(t.Context(), "conv-1")
	if len(msgs) != 1 || msgs[len(msgs)-1].Role != "user" {
		t.Fatalf("foreign finalize mutated the thread: %+v", msgs)
	}

	// Owner: appends the partial as the terminal assistant row.
	if ok, err := st.FinalizeStopped(t.Context(), "conv-1", "owner", "partial reply"); err != nil || !ok {
		t.Fatalf("owner finalize: ok=%v err=%v", ok, err)
	}
	msgs, _ = st.ListMessages(t.Context(), "conv-1")
	if len(msgs) != 2 || msgs[1].Role != "assistant" || msgs[1].Content != "partial reply" {
		t.Fatalf("owner finalize did not append the partial: %+v", msgs)
	}

	// Idempotent: a second finalize is a no-op (last row is already assistant).
	if ok, err := st.FinalizeStopped(t.Context(), "conv-1", "owner", "again"); err != nil || ok {
		t.Fatalf("second finalize: ok=%v err=%v (must be no-op)", ok, err)
	}
	if msgs, _ := st.ListMessages(t.Context(), "conv-1"); len(msgs) != 2 {
		t.Fatalf("second finalize appended a duplicate: %+v", msgs)
	}
}
