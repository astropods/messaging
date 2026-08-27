package sqlite

import (
	"testing"
	"time"
)

func savedMsgs(contents ...string) []SavedMessage {
	out := make([]SavedMessage, 0, len(contents))
	for i, c := range contents {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		out = append(out, SavedMessage{Role: role, Author: "Ada", Content: c})
	}
	return out
}

func save(t *testing.T, st *Store, req SaveRequest) (string, SaveStatus) {
	t.Helper()
	if req.UserID == "" {
		req.UserID = "user_1"
	}
	if req.IdempotencyKey == "" {
		req.IdempotencyKey = "k1"
	}
	id, status, err := st.SaveConversation(t.Context(), req)
	if err != nil {
		t.Fatalf("SaveConversation: %v", err)
	}
	return id, status
}

func contents(t *testing.T, st *Store, id string) []string {
	t.Helper()
	msgs, err := st.ListMessages(t.Context(), id)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.Content)
	}
	return out
}

func TestDeriveSavedConversationID_StableAndScopedToUser(t *testing.T) {
	a := DeriveSavedConversationID("user_1", "slack:C1:111.0001")
	if a != DeriveSavedConversationID("user_1", "slack:C1:111.0001") {
		t.Fatal("same user and key must derive the same id")
	}
	if a == DeriveSavedConversationID("user_2", "slack:C1:111.0001") {
		t.Fatal("a second participant must get their own copy, not share one")
	}
	if a == DeriveSavedConversationID("user_1", "slack:C1:222.0001") {
		t.Fatal("a different thread must be a different conversation")
	}
}

func TestSaveConversation_CreatesOwnedConversation(t *testing.T) {
	st := newTestStore(t)

	id, status := save(t, st, SaveRequest{
		Title: "Thread", SourceLabel: "#eng", SourceURL: "https://slack/x",
		Messages: savedMsgs("hello", "hi back"),
	})
	if status != SaveCreated {
		t.Fatalf("status = %q, want created", status)
	}

	conv, err := st.Get(t.Context(), id)
	if err != nil || conv == nil {
		t.Fatalf("Get: %v", err)
	}
	if conv.UserID != "user_1" || conv.Title != "Thread" {
		t.Fatalf("owner/title wrong: %+v", conv)
	}
	if conv.SourceLabel != "#eng" || conv.SourceURL != "https://slack/x" {
		t.Fatalf("source not persisted: %+v", conv)
	}

	msgs, _ := st.ListMessages(t.Context(), id)
	if len(msgs) != 2 || msgs[1].Role != "assistant" {
		t.Fatalf("messages wrong: %+v", msgs)
	}
	if msgs[0].Author != "Ada" {
		t.Fatal("author must survive so a multi-party thread does not read as one speaker")
	}

	// The copy has to reach the sidebar, which excludes conversations with no
	// messages and orders by recency.
	if list, err := st.ListByUser(t.Context(), "user_1"); err != nil || len(list) != 1 {
		t.Fatalf("ListByUser: %d %v", len(list), err)
	}
}

// Re-saving a copy the user has not touched refreshes it, so an agent that
// re-reads its source and re-sends the whole thread propagates edits and
// deletions instead of accumulating duplicates.
func TestSaveConversation_RefreshesAPristineCopy(t *testing.T) {
	st := newTestStore(t)

	first, _ := save(t, st, SaveRequest{Title: "Thread", Messages: savedMsgs("one", "two")})
	second, status := save(t, st, SaveRequest{Title: "Thread edited", Messages: savedMsgs("one edited")})

	if status != SaveReplaced {
		t.Fatalf("status = %q, want replaced", status)
	}
	if first != second {
		t.Fatalf("same key must resolve to the same conversation: %s vs %s", first, second)
	}
	if got := contents(t, st, first); len(got) != 1 || got[0] != "one edited" {
		t.Fatalf("expected the copy refreshed, got %v", got)
	}
	msgs, _ := st.ListMessages(t.Context(), first)
	if msgs[0].Seq != 1 {
		t.Fatalf("a refreshed copy must restart at seq 1, got %d", msgs[0].Seq)
	}
	if conv, _ := st.Get(t.Context(), first); conv.Title != "Thread edited" {
		t.Fatalf("title not refreshed: %q", conv.Title)
	}
}

// A saved copy is an ordinary conversation, so the user can chat in it. The next
// save must not throw their turns away without the agent asking for that.
func TestSaveConversation_SkipsACopyTheUserRepliedIn(t *testing.T) {
	st := newTestStore(t)

	id, _ := save(t, st, SaveRequest{Title: "Thread", Messages: savedMsgs("slack one")})
	if _, err := st.AppendMessage(t.Context(), id, "user_1", "user", "summarise this", ""); err != nil {
		t.Fatalf("user turn: %v", err)
	}
	if _, err := st.UpsertAssistantProgress(t.Context(), id, "here you go", ""); err != nil {
		t.Fatalf("assistant turn: %v", err)
	}

	_, status := save(t, st, SaveRequest{Title: "Thread", Messages: savedMsgs("slack one", "slack two")})
	if status != SaveSkippedDiverged {
		t.Fatalf("status = %q, want skipped_diverged", status)
	}

	got := contents(t, st, id)
	if len(got) != 3 || got[1] != "summarise this" || got[2] != "here you go" {
		t.Fatalf("the user's own turns must survive, got %v", got)
	}
}

// An answered form is the user's work too, even when they typed no message.
func TestSaveConversation_InteractionsCountAsDivergence(t *testing.T) {
	st := newTestStore(t)

	id, _ := save(t, st, SaveRequest{Messages: savedMsgs("slack one")})
	if _, err := st.AppendInteraction(t.Context(), id, "user_1", nil); err != nil {
		t.Fatalf("interaction: %v", err)
	}

	if _, status := save(t, st, SaveRequest{Messages: savedMsgs("slack two")}); status != SaveSkippedDiverged {
		t.Fatalf("status = %q, want skipped_diverged", status)
	}
}

// REPLACE is the agent stating it means to discard the user's turns, so the
// platform stops protecting them. Nothing reaches this path by default.
func TestSaveConversation_ReplaceOverwritesADivergedCopy(t *testing.T) {
	st := newTestStore(t)

	id, _ := save(t, st, SaveRequest{Messages: savedMsgs("slack one")})
	if _, err := st.AppendMessage(t.Context(), id, "user_1", "user", "my own note", ""); err != nil {
		t.Fatalf("user turn: %v", err)
	}

	_, status := save(t, st, SaveRequest{
		Messages: savedMsgs("slack one", "slack two"), OnConflict: OnConflictReplace,
	})
	if status != SaveReplaced {
		t.Fatalf("status = %q, want replaced", status)
	}
	if got := contents(t, st, id); len(got) != 2 || got[0] != "slack one" {
		t.Fatalf("expected an overwrite, got %v", got)
	}
}

// APPEND is how an agent syncs incrementally: it sends only the new turns and
// they land after whatever the user has written.
func TestSaveConversation_AppendAddsAfterTheUsersTurns(t *testing.T) {
	st := newTestStore(t)

	id, _ := save(t, st, SaveRequest{Messages: savedMsgs("slack one")})
	if _, err := st.AppendMessage(t.Context(), id, "user_1", "user", "my own note", ""); err != nil {
		t.Fatalf("user turn: %v", err)
	}

	_, status := save(t, st, SaveRequest{
		Messages: savedMsgs("slack two"), OnConflict: OnConflictAppend,
	})
	if status != SaveAppended {
		t.Fatalf("status = %q, want appended", status)
	}
	got := contents(t, st, id)
	if len(got) != 3 || got[0] != "slack one" || got[1] != "my own note" || got[2] != "slack two" {
		t.Fatalf("expected the new turn appended in order, got %v", got)
	}
}

// Delete is how a user opts out of an agent that saves on every source message.
// It has to survive the next save or the copy comes straight back.
func TestSaveConversation_DeletedCopyIsNeverRecreated(t *testing.T) {
	st := newTestStore(t)

	id, _ := save(t, st, SaveRequest{Messages: savedMsgs("one")})
	if _, deleted, err := st.SoftDelete(t.Context(), id, "user_1"); err != nil || !deleted {
		t.Fatalf("SoftDelete: deleted=%v err=%v", deleted, err)
	}

	for _, mode := range []OnConflict{OnConflictSkip, OnConflictReplace, OnConflictAppend} {
		gotID, status := save(t, st, SaveRequest{Messages: savedMsgs("two"), OnConflict: mode})
		if status != SaveSkippedDeleted {
			t.Fatalf("mode %v: status = %q, want skipped_deleted", mode, status)
		}
		if gotID != id {
			t.Fatalf("expected the same derived id back, got %s", gotID)
		}
	}
	if conv, _ := st.Get(t.Context(), id); conv != nil {
		t.Fatal("conversation should still read as deleted")
	}
}

func TestSaveConversation_RefusesForeignConversation(t *testing.T) {
	st := newTestStore(t)

	id := DeriveSavedConversationID("user_1", "k1")
	if err := st.Upsert(t.Context(), id, "user_2", "someone else's chat"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, status := save(t, st, SaveRequest{Messages: savedMsgs("x"), OnConflict: OnConflictReplace}); status != SaveSkippedConflict {
		t.Fatalf("status = %q, want skipped_conflict", status)
	}
	conv, _ := st.Get(t.Context(), id)
	if conv == nil || conv.Title != "someone else's chat" || conv.UserID != "user_2" {
		t.Fatalf("a colliding id must not overwrite the owner's conversation: %+v", conv)
	}
}

func TestSaveConversation_KeepsNewestWithinTheMessageCap(t *testing.T) {
	st := newTestStore(t)

	msgs := make([]SavedMessage, MaxMessagesPerConversation+5)
	for i := range msgs {
		msgs[i] = SavedMessage{Role: "user", Content: string(rune('a' + i%26))}
	}
	msgs[len(msgs)-1].Content = "newest"

	id, _ := save(t, st, SaveRequest{Title: "Long", Messages: msgs})
	stored := contents(t, st, id)
	if len(stored) != MaxMessagesPerConversation {
		t.Fatalf("expected %d messages, got %d", MaxMessagesPerConversation, len(stored))
	}
	if stored[len(stored)-1] != "newest" {
		t.Fatal("truncation must drop the oldest turns, not the newest")
	}
}

func TestSaveConversation_RequiresUserAndKey(t *testing.T) {
	st := newTestStore(t)
	if _, _, err := st.SaveConversation(t.Context(), SaveRequest{IdempotencyKey: "k1"}); err == nil {
		t.Fatal("expected an error for an empty user id")
	}
	if _, _, err := st.SaveConversation(t.Context(), SaveRequest{UserID: "user_1"}); err == nil {
		t.Fatal("expected an error for an empty idempotency key")
	}
}

func TestSaveConversation_UsesSourceTimestamps(t *testing.T) {
	st := newTestStore(t)
	when := time.Now().Add(-72 * time.Hour).Truncate(time.Millisecond)

	id, _ := save(t, st, SaveRequest{
		Messages: []SavedMessage{{Role: "user", Content: "old", Timestamp: when}},
	})

	var createdMs int64
	if err := st.db.QueryRowContext(t.Context(),
		`SELECT created_at FROM messages WHERE conversation_id = ?`, id).Scan(&createdMs); err != nil {
		t.Fatalf("read created_at: %v", err)
	}
	if createdMs != when.UnixMilli() {
		t.Fatalf("expected the source timestamp %d, got %d", when.UnixMilli(), createdMs)
	}
}

// A Slack thread ends on a human turn, so a naive "last message is the user's"
// test reads every copy as a turn in flight and the UI spins forever.
func TestSaveConversation_CopyDoesNotReadAsAnInFlightTurn(t *testing.T) {
	st := newTestStore(t)

	id, _ := save(t, st, SaveRequest{Title: "Thread", Messages: savedMsgs("only a user turn")})

	list, err := st.ListByUser(t.Context(), "user_1")
	if err != nil || len(list) != 1 {
		t.Fatalf("ListByUser: %d %v", len(list), err)
	}
	if list[0].AssistantStreaming {
		t.Error("a saved copy must not report an assistant reply in flight")
	}
	if _, _, _, lastRole, err := st.PageMessages(t.Context(), id, 10, 0); err != nil || lastRole == "user" {
		t.Errorf("PageMessages lastRole = %q, want it blanked for a copy (%v)", lastRole, err)
	}
}

// The startup reaper appends a terminal empty assistant row to conversations
// whose last message is the user's. Left unguarded it would do that to every
// saved copy on every restart.
func TestSaveConversation_StartupReaperLeavesCopiesAlone(t *testing.T) {
	st := newTestStore(t)

	id, _ := save(t, st, SaveRequest{Title: "Thread", Messages: savedMsgs("only a user turn")})

	n, err := st.ReapDanglingUserTurns(t.Context())
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if n != 0 {
		t.Errorf("reaped %d conversations, want 0", n)
	}
	if got := contents(t, st, id); len(got) != 1 {
		t.Errorf("copy gained a row: %v", got)
	}
}

// A saved copy is an ordinary conversation, so the user can rename it. An agent
// that re-saves on every source message must not undo that.
func TestSaveConversation_UserRenameSurvivesAResave(t *testing.T) {
	st := newTestStore(t)

	id, _ := save(t, st, SaveRequest{Title: "Slack thread", Messages: savedMsgs("one")})
	if ok, err := st.SetTitle(t.Context(), id, "user_1", "Deploy incident, Aug 27"); err != nil || !ok {
		t.Fatalf("SetTitle: %v", err)
	}

	save(t, st, SaveRequest{Title: "Slack thread", Messages: savedMsgs("one", "two")})

	conv, _ := st.Get(t.Context(), id)
	if conv.Title != "Deploy incident, Aug 27" {
		t.Fatalf("title = %q, want the user's rename kept", conv.Title)
	}
}

// Title is optional. Omitting it on a refresh must not blank the one already there.
func TestSaveConversation_EmptyTitleNeverBlanksAnExistingOne(t *testing.T) {
	st := newTestStore(t)

	id, _ := save(t, st, SaveRequest{Title: "Slack thread", Messages: savedMsgs("one")})
	save(t, st, SaveRequest{Messages: savedMsgs("one", "two")})

	conv, _ := st.Get(t.Context(), id)
	if conv.Title != "Slack thread" {
		t.Fatalf("title = %q, want it preserved", conv.Title)
	}
}

// The agent still owns the title while the user has not touched it, so a thread
// whose subject changes can be re-titled.
func TestSaveConversation_AgentCanRetitleAnUntouchedCopy(t *testing.T) {
	st := newTestStore(t)

	id, _ := save(t, st, SaveRequest{Title: "Slack thread", Messages: savedMsgs("one")})
	save(t, st, SaveRequest{Title: "#eng-support: deploy failure", Messages: savedMsgs("one", "two")})

	conv, _ := st.Get(t.Context(), id)
	if conv.Title != "#eng-support: deploy failure" {
		t.Fatalf("title = %q, want the agent's new title", conv.Title)
	}
}
