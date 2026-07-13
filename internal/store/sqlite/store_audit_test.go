package sqlite

// Adversarial audit of the chat store: these tests deliberately try to break the
// invariants the feature relies on (ownership, terminal-turn semantics, soft
// delete, caps, concurrency). They are written to FAIL against a regression, not
// to pin current behavior.
//
//	go test ./internal/store/sqlite -run TestAudit -race -v

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

// A send to a soft-deleted conversation must not revive it: EnsureForSend reports
// not-owned and the tombstone stays. (Deletes are terminal.)
func TestAudit_SoftDeletedNotRevivedBySend(t *testing.T) {
	st := newTestStore(t)
	ctx := t.Context()
	if _, err := st.EnsureForSend(ctx, "c", "owner", "t"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, _, err := st.SoftDelete(ctx, "c", "owner"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	owned, err := st.EnsureForSend(ctx, "c", "owner", "revive?")
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if owned {
		t.Fatal("send to a soft-deleted conversation reported owned (would revive it)")
	}
	if conv, _ := st.Get(ctx, "c"); conv != nil {
		t.Fatalf("soft-deleted conversation was revived: %+v", conv)
	}
}

// A late assistant write (progressive persist) must not resurrect or write into a
// soft-deleted conversation.
func TestAudit_UpsertAssistantProgressNoOpOnDeleted(t *testing.T) {
	st := newTestStore(t)
	ctx := t.Context()
	if _, err := st.EnsureForSend(ctx, "c", "owner", "t"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := st.AppendMessage(ctx, "c", "owner", "user", "hi"); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, _, err := st.SoftDelete(ctx, "c", "owner"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	id, err := st.UpsertAssistantProgress(ctx, "c", "late chunk")
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if id != "" {
		t.Fatalf("assistant progress wrote to a deleted conversation (id=%q)", id)
	}
	if conv, _ := st.Get(ctx, "c"); conv != nil {
		t.Fatalf("assistant progress revived a deleted conversation: %+v", conv)
	}
}

// The regression the FinalizeTerminal design guards against: a spurious error
// arriving AFTER a turn completed (buffer cleared, so partial is empty) must NOT
// blank the finished assistant reply.
func TestAudit_FinalizeTerminalDoesNotBlankCompletedReply(t *testing.T) {
	st := newTestStore(t)
	ctx := t.Context()
	if _, err := st.EnsureForSend(ctx, "c", "owner", "t"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := st.AppendMessage(ctx, "c", "owner", "user", "q"); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	// A completed assistant reply.
	if _, err := st.UpsertAssistantProgress(ctx, "c", "the full answer"); err != nil {
		t.Fatalf("assistant: %v", err)
	}

	// Spurious error after completion, with an empty partial (tracker cleared).
	appended, err := st.FinalizeTerminal(ctx, "c", "")
	if err != nil {
		t.Fatalf("finalize terminal: %v", err)
	}
	if appended {
		t.Fatal("FinalizeTerminal appended after a completed turn (should be a no-op)")
	}
	msgs, _ := st.ListMessages(ctx, "c")
	if len(msgs) != 2 {
		t.Fatalf("want 2 messages, got %d: %+v", len(msgs), msgs)
	}
	if last := msgs[len(msgs)-1]; last.Role != "assistant" || last.Content != "the full answer" {
		t.Fatalf("completed reply was blanked/altered: %+v", last)
	}
}

// FinalizeTerminal must resolve assistant_streaming for a turn that errored before
// any assistant row was written (last message still the user's).
func TestAudit_FinalizeTerminalResolvesStreamingOnErroredTurn(t *testing.T) {
	st := newTestStore(t)
	ctx := t.Context()
	if _, err := st.EnsureForSend(ctx, "c", "owner", "t"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := st.AppendMessage(ctx, "c", "owner", "user", "q"); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	// Precondition: derived streaming true (last is user).
	if convs, _ := st.ListByUser(ctx, "owner"); len(convs) != 1 || !convs[0].AssistantStreaming {
		t.Fatalf("precondition: expected streaming=true, got %+v", convs)
	}

	appended, err := st.FinalizeTerminal(ctx, "c", "partial the user saw")
	if err != nil || !appended {
		t.Fatalf("finalize terminal: appended=%v err=%v", appended, err)
	}
	if convs, _ := st.ListByUser(ctx, "owner"); len(convs) != 1 || convs[0].AssistantStreaming {
		t.Fatalf("errored turn still reports streaming: %+v", convs)
	}
}

// FinalizeStopped must be a no-op on a soft-deleted conversation (no resurrection,
// no stray assistant row).
func TestAudit_FinalizeStoppedNoOpOnDeleted(t *testing.T) {
	st := newTestStore(t)
	ctx := t.Context()
	if _, err := st.EnsureForSend(ctx, "c", "owner", "t"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := st.AppendMessage(ctx, "c", "owner", "user", "q"); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, _, err := st.SoftDelete(ctx, "c", "owner"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if ok, err := st.FinalizeStopped(ctx, "c", "owner", "partial"); err != nil || ok {
		t.Fatalf("finalize on deleted: ok=%v err=%v (must be no-op)", ok, err)
	}
	if conv, _ := st.Get(ctx, "c"); conv != nil {
		t.Fatalf("finalize revived a deleted conversation: %+v", conv)
	}
}

// The per-conversation message cap is enforced with a slot reserved for the
// assistant reply: a user turn is rejected once only the final slot remains, but
// that slot still admits one assistant reply (reaching the hard cap), after which
// even assistant appends are rejected. Nothing beyond the hard cap is stored.
func TestAudit_MessageLimitEnforced(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	if _, err := st.EnsureForSend(ctx, "c", "owner", "t"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Fill user turns up to the reserved cap (one slot held back for a reply).
	for i := 0; i < MaxMessagesPerConversation-1; i++ {
		if _, err := st.AppendMessage(ctx, "c", "owner", "user", "m"); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	// A further user turn is rejected — no room left for its assistant reply.
	if _, err := st.AppendMessage(ctx, "c", "owner", "user", "over"); !errors.Is(err, ErrMessageLimitReached) {
		t.Fatalf("user append at reserved cap: want ErrMessageLimitReached, got %v", err)
	}
	// The reserved final slot still admits one assistant reply (hard cap reached).
	if _, err := st.AppendMessage(ctx, "c", "owner", "assistant", "reply"); err != nil {
		t.Fatalf("assistant append into reserved slot: %v", err)
	}
	// Now truly full: even an assistant append is rejected.
	if _, err := st.AppendMessage(ctx, "c", "owner", "assistant", "over"); !errors.Is(err, ErrMessageLimitReached) {
		t.Fatalf("assistant append past hard cap: want ErrMessageLimitReached, got %v", err)
	}
	if msgs, _ := st.ListMessages(ctx, "c"); len(msgs) != MaxMessagesPerConversation {
		t.Fatalf("stored %d messages, want hard cap %d", len(msgs), MaxMessagesPerConversation)
	}
}

// Oversized content is truncated at a rune boundary (never split mid-rune).
func TestAudit_ContentTruncatedAtRuneCap(t *testing.T) {
	st := newTestStore(t)
	ctx := t.Context()
	if _, err := st.EnsureForSend(ctx, "c", "owner", "t"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Multibyte runes so a byte-based cut would corrupt UTF-8.
	oversized := strings.Repeat("é", MaxMessageContentRunes+50)
	if _, err := st.AppendMessage(ctx, "c", "owner", "user", oversized); err != nil {
		t.Fatalf("append: %v", err)
	}
	msgs, _ := st.ListMessages(ctx, "c")
	got := msgs[len(msgs)-1].Content
	if !utf8.ValidString(got) {
		t.Fatal("stored content is not valid UTF-8 (truncation split a rune)")
	}
	if n := utf8.RuneCountInString(got); n != MaxMessageContentRunes {
		t.Fatalf("stored %d runes, want cap %d", n, MaxMessageContentRunes)
	}
}

func TestAudit_TruncateRunesMultibyte(t *testing.T) {
	got := TruncateRunes(strings.Repeat("😀", 5), 3)
	if !utf8.ValidString(got) {
		t.Fatal("TruncateRunes produced invalid UTF-8")
	}
	if n := utf8.RuneCountInString(got); n != 3 {
		t.Fatalf("TruncateRunes cap=3 gave %d runes", n)
	}
	// Under the cap is returned unchanged.
	if TruncateRunes("hi", 10) != "hi" {
		t.Fatal("TruncateRunes altered an under-cap string")
	}
}

// Concurrent appends across DIFFERENT conversations must all land with correct
// per-conversation contiguous seqs (global single-connection serialization must
// not drop or deadlock).
func TestAudit_ConcurrentAppendsAcrossConversations(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	const convs, perConv = 8, 20
	for c := 0; c < convs; c++ {
		if _, err := st.EnsureForSend(ctx, "c"+strconv.Itoa(c), "owner", "t"); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	var wg sync.WaitGroup
	errCh := make(chan error, convs*perConv)
	for c := 0; c < convs; c++ {
		for i := 0; i < perConv; i++ {
			wg.Add(1)
			go func(cid string) {
				defer wg.Done()
				if _, err := st.AppendMessage(ctx, cid, "owner", "user", "m"); err != nil {
					errCh <- err
				}
			}("c" + strconv.Itoa(c))
		}
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent append dropped a message: %v", err)
	}
	for c := 0; c < convs; c++ {
		msgs, _ := st.ListMessages(ctx, "c"+strconv.Itoa(c))
		if len(msgs) != perConv {
			t.Fatalf("conversation c%d: got %d messages, want %d", c, len(msgs), perConv)
		}
		seen := make(map[int]bool)
		for _, m := range msgs {
			if m.Seq < 1 || m.Seq > perConv || seen[m.Seq] {
				t.Fatalf("conversation c%d: bad/duplicate seq %d", c, m.Seq)
			}
			seen[m.Seq] = true
		}
	}
}

// Concurrent stop-finalize and soft-delete on the same conversation must stay
// consistent: no panic, the conversation ends deleted, and at most one assistant
// row was appended (never a torn/duplicate state).
func TestAudit_ConcurrentFinalizeAndSoftDelete(t *testing.T) {
	ctx := context.Background()
	for i := 0; i < 40; i++ {
		st := newTestStore(t)
		cid := "c" + strconv.Itoa(i)
		if _, err := st.EnsureForSend(ctx, cid, "owner", "t"); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if _, err := st.AppendMessage(ctx, cid, "owner", "user", "q"); err != nil {
			t.Fatalf("seed user: %v", err)
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _, _ = st.FinalizeStopped(ctx, cid, "owner", "partial") }()
		go func() { defer wg.Done(); _, _, _ = st.SoftDelete(ctx, cid, "owner") }()
		wg.Wait()

		if conv, _ := st.Get(ctx, cid); conv != nil {
			t.Fatalf("iter %d: conversation not deleted after SoftDelete: %+v", i, conv)
		}
		msgs, _ := st.ListMessages(ctx, cid)
		assistants := 0
		for _, m := range msgs {
			if m.Role == "assistant" {
				assistants++
			}
		}
		if assistants > 1 {
			t.Fatalf("iter %d: %d assistant rows (torn finalize): %+v", i, assistants, msgs)
		}
	}
}

// ListByUser excludes soft-deleted conversations and orders by recency.
func TestAudit_ListExcludesDeletedAndOrdersByRecency(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	for _, id := range []string{"a", "b", "d"} {
		if _, err := st.EnsureForSend(ctx, id, "owner", id); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
		if _, err := st.AppendMessage(ctx, id, "owner", "user", "hi"); err != nil {
			t.Fatalf("seed msg %s: %v", id, err)
		}
	}
	// Bump "a" to most-recent, then delete "b".
	if _, err := st.EnsureForSend(ctx, "a", "owner", "a"); err != nil {
		t.Fatalf("touch a: %v", err)
	}
	if _, _, err := st.SoftDelete(ctx, "b", "owner"); err != nil {
		t.Fatalf("delete b: %v", err)
	}

	convs, err := st.ListByUser(ctx, "owner")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	ids := make([]string, len(convs))
	for i, c := range convs {
		ids[i] = c.ConversationID
		if c.ConversationID == "b" {
			t.Fatal("soft-deleted conversation appeared in the list")
		}
	}
	if len(ids) != 2 || ids[0] != "a" {
		t.Fatalf("expected [a, d] most-recent-first, got %v", ids)
	}
}

// ListByUser excludes conversations with no messages (a pre-created "new chat"
// that was never sent to), so empty rows don't pile up in the sidebar.
func TestAudit_ListExcludesEmptyConversations(t *testing.T) {
	st := newTestStore(t)
	ctx := t.Context()
	// Pre-created, never sent to (mirrors HandleCreateConversation's Upsert).
	if err := st.Upsert(ctx, "empty", "owner", ""); err != nil {
		t.Fatalf("pre-create: %v", err)
	}
	// A real conversation with a message.
	if _, err := st.EnsureForSend(ctx, "full", "owner", "Full"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := st.AppendMessage(ctx, "full", "owner", "user", "hi"); err != nil {
		t.Fatalf("seed msg: %v", err)
	}

	convs, err := st.ListByUser(ctx, "owner")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(convs) != 1 || convs[0].ConversationID != "full" {
		t.Fatalf("expected only [full], got %+v", convs)
	}
}

// Cancel after progressive persistence: FinalizeStopped grows the trailing
// assistant row to the fuller buffered partial (the persisted snapshot lags the
// buffer), but never shrinks it.
func TestAudit_FinalizeStoppedGrowsButNeverShrinks(t *testing.T) {
	st := newTestStore(t)
	ctx := t.Context()
	if _, err := st.EnsureForSend(ctx, "c", "owner", "t"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := st.AppendMessage(ctx, "c", "owner", "user", "q"); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	// Progressive snapshot (lags what the user saw).
	if _, err := st.UpsertAssistantProgress(ctx, "c", "partial so"); err != nil {
		t.Fatalf("progress: %v", err)
	}

	// Stop with the fuller buffered partial → grows the row.
	if ok, err := st.FinalizeStopped(ctx, "c", "owner", "partial so far and then some more"); err != nil || !ok {
		t.Fatalf("finalize grow: ok=%v err=%v", ok, err)
	}
	msgs, _ := st.ListMessages(ctx, "c")
	if len(msgs) != 2 || msgs[1].Content != "partial so far and then some more" {
		t.Fatalf("cancel did not grow to the buffered partial: %+v", msgs)
	}

	// A second stop with a SHORTER partial must not shrink the persisted reply.
	if _, err := st.FinalizeStopped(ctx, "c", "owner", "short"); err != nil {
		t.Fatalf("finalize shrink attempt: %v", err)
	}
	msgs, _ = st.ListMessages(ctx, "c")
	if len(msgs) != 2 || msgs[1].Content != "partial so far and then some more" {
		t.Fatalf("cancel shrank a finished reply: %+v", msgs)
	}
}
