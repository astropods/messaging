package sqlite

// Covers ReapDanglingUserTurns: on startup, conversations whose latest message is
// still the user's (a turn interrupted before its assistant row was persisted)
// are finalized with a terminal assistant row so they stop deriving
// assistant_streaming. Completed, empty, and deleted conversations are untouched.
//
//	go test ./internal/store/sqlite -run TestReapDanglingUserTurns -v

import (
	"context"
	"testing"
)

func TestReapDanglingUserTurns(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	ensure := func(id string) {
		t.Helper()
		if _, err := st.EnsureForSend(ctx, id, "owner", "t"); err != nil {
			t.Fatalf("ensure %s: %v", id, err)
		}
	}
	appendUser := func(id string) {
		t.Helper()
		if _, err := st.AppendMessage(ctx, id, "owner", "user", "hi"); err != nil {
			t.Fatalf("append %s: %v", id, err)
		}
	}

	// a: user-last (interrupted) -> reaped.
	ensure("a")
	appendUser("a")
	// b: assistant-last (completed) -> untouched.
	ensure("b")
	appendUser("b")
	if _, err := st.UpsertAssistantProgress(ctx, "b", "reply"); err != nil {
		t.Fatalf("progress b: %v", err)
	}
	// c: empty (created, never sent) -> untouched.
	ensure("c")
	// d: soft-deleted, user-last -> untouched (not resurrected).
	ensure("d")
	appendUser("d")
	if _, err := st.SoftDelete(ctx, "d", "owner"); err != nil {
		t.Fatalf("delete d: %v", err)
	}

	n, err := st.ReapDanglingUserTurns(ctx)
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if n != 1 {
		t.Fatalf("finalized %d, want 1 (only the interrupted user-last conversation)", n)
	}

	if msgs, _ := st.ListMessages(ctx, "a"); len(msgs) != 2 || msgs[len(msgs)-1].Role != "assistant" {
		t.Fatalf("a not finalized with a terminal assistant row: %+v", msgs)
	}
	if msgs, _ := st.ListMessages(ctx, "b"); len(msgs) != 2 || msgs[len(msgs)-1].Role != "assistant" || msgs[len(msgs)-1].Content != "reply" {
		t.Fatalf("completed conversation b was altered: %+v", msgs)
	}
	if msgs, _ := st.ListMessages(ctx, "c"); len(msgs) != 0 {
		t.Fatalf("empty conversation c was altered: %+v", msgs)
	}

	// Idempotent: a is now assistant-last, so a second pass finalizes nothing.
	if n2, err := st.ReapDanglingUserTurns(ctx); err != nil || n2 != 0 {
		t.Fatalf("second reap should be a no-op: n=%d err=%v", n2, err)
	}
}
