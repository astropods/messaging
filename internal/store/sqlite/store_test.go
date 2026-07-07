package sqlite

import (
	"path/filepath"
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
