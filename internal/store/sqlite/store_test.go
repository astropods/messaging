package sqlite

import (
	"path/filepath"
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

	if err := st.Upsert("conv-1", "user-1", "original"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	t.Run("renames owned conversation", func(t *testing.T) {
		ok, err := st.SetTitle("conv-1", "user-1", "renamed")
		if err != nil {
			t.Fatalf("SetTitle: %v", err)
		}
		if !ok {
			t.Fatal("expected rename to apply")
		}
		conv, _ := st.Get("conv-1")
		if conv == nil || conv.Title != "renamed" {
			t.Fatalf("title = %+v, want renamed", conv)
		}
	})

	t.Run("foreign user does not match", func(t *testing.T) {
		ok, err := st.SetTitle("conv-1", "user-2", "hijacked")
		if err != nil {
			t.Fatalf("SetTitle: %v", err)
		}
		if ok {
			t.Fatal("expected no rows affected for foreign user")
		}
		conv, _ := st.Get("conv-1")
		if conv.Title != "renamed" {
			t.Fatalf("foreign rename mutated title: %q", conv.Title)
		}
	})

	t.Run("missing conversation is not created", func(t *testing.T) {
		ok, err := st.SetTitle("conv-missing", "user-1", "new")
		if err != nil {
			t.Fatalf("SetTitle: %v", err)
		}
		if ok {
			t.Fatal("expected no rows affected for missing conversation")
		}
		if conv, _ := st.Get("conv-missing"); conv != nil {
			t.Fatalf("SetTitle created a conversation: %+v", conv)
		}
	})

	t.Run("deleted conversation does not match", func(t *testing.T) {
		if err := st.Upsert("conv-del", "user-1", "orig"); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if _, err := st.SoftDelete("conv-del", "user-1"); err != nil {
			t.Fatalf("soft delete: %v", err)
		}
		ok, err := st.SetTitle("conv-del", "user-1", "revived")
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
	if err := st.EnsureForSend("conv-1", "user-1", "seed"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	const n = 50
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := st.AppendMessage("conv-1", "user-1", "user", "m"); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("append failed (message dropped under concurrency): %v", err)
	}

	msgs, err := st.ListMessages("conv-1")
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

// EnsureForSend fills the derived title when a conversation was pre-created with
// an empty title (the create-then-send flow) but never overwrites a title that
// is already set, and titles a brand-new conversation on create.
func TestEnsureForSendFillsEmptyTitleOnce(t *testing.T) {
	st := newTestStore(t)

	// Simulate HandleCreateConversation: pre-create with an empty title.
	if err := st.Upsert("conv-1", "user-1", ""); err != nil {
		t.Fatalf("pre-create: %v", err)
	}
	if conv, _ := st.Get("conv-1"); conv == nil || conv.Title != "" {
		t.Fatalf("precondition: want empty title, got %+v", conv)
	}

	// First send fills the derived title.
	if err := st.EnsureForSend("conv-1", "user-1", "Derived"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if conv, _ := st.Get("conv-1"); conv == nil || conv.Title != "Derived" {
		t.Fatalf("title not filled on first send: %+v", conv)
	}

	// Later sends must not overwrite an existing title.
	if err := st.EnsureForSend("conv-1", "user-1", "Later"); err != nil {
		t.Fatalf("ensure 2: %v", err)
	}
	if conv, _ := st.Get("conv-1"); conv == nil || conv.Title != "Derived" {
		t.Fatalf("existing title overwritten on later send: %+v", conv)
	}

	// A brand-new conversation is created already titled.
	if err := st.EnsureForSend("conv-2", "user-1", "Fresh"); err != nil {
		t.Fatalf("ensure new: %v", err)
	}
	if conv, _ := st.Get("conv-2"); conv == nil || conv.Title != "Fresh" {
		t.Fatalf("new conversation not titled on create: %+v", conv)
	}
}
