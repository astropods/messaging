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

	id, saved, err := st.SaveConversation(t.Context(), "user_1", "k1", "Thread", "#eng", "https://slack/x",
		savedMsgs("hello", "hi back"))
	if err != nil || !saved {
		t.Fatalf("SaveConversation: saved=%v err=%v", saved, err)
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

	msgs, err := st.ListMessages(t.Context(), id)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 2 || msgs[0].Content != "hello" || msgs[1].Role != "assistant" {
		t.Fatalf("messages wrong: %+v", msgs)
	}
	if msgs[0].Author != "Ada" {
		t.Fatal("author must survive so a multi-party thread does not read as one speaker")
	}

	// The copy has to reach the sidebar, which excludes conversations with no
	// messages and orders by recency.
	list, err := st.ListByUser(t.Context(), "user_1")
	if err != nil || len(list) != 1 {
		t.Fatalf("ListByUser: %d %v", len(list), err)
	}
}

// A re-save under the same key replaces the copy rather than appending, so an
// agent that re-reads its source and re-sends the whole thread propagates edits
// and deletions instead of accumulating duplicates.
func TestSaveConversation_RepeatKeyReplacesContents(t *testing.T) {
	st := newTestStore(t)

	first, _, err := st.SaveConversation(t.Context(), "user_1", "k1", "Thread", "#eng", "", savedMsgs("one", "two"))
	if err != nil {
		t.Fatalf("first save: %v", err)
	}
	second, saved, err := st.SaveConversation(t.Context(), "user_1", "k1", "Thread edited", "#eng", "",
		savedMsgs("one edited"))
	if err != nil || !saved {
		t.Fatalf("second save: saved=%v err=%v", saved, err)
	}
	if first != second {
		t.Fatalf("same key must resolve to the same conversation: %s vs %s", first, second)
	}

	msgs, _ := st.ListMessages(t.Context(), first)
	if len(msgs) != 1 || msgs[0].Content != "one edited" {
		t.Fatalf("expected the copy replaced, got %+v", msgs)
	}
	if msgs[0].Seq != 1 {
		t.Fatalf("replaced copy must restart at seq 1, got %d", msgs[0].Seq)
	}
	conv, _ := st.Get(t.Context(), first)
	if conv.Title != "Thread edited" {
		t.Fatalf("title not refreshed: %q", conv.Title)
	}
}

// Delete is how a user opts out of an agent that saves on every source message.
// It has to survive the next save or the copy comes straight back.
func TestSaveConversation_DeletedCopyIsNeverRecreated(t *testing.T) {
	st := newTestStore(t)

	id, _, err := st.SaveConversation(t.Context(), "user_1", "k1", "Thread", "#eng", "", savedMsgs("one"))
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, deleted, err := st.SoftDelete(t.Context(), id, "user_1"); err != nil || !deleted {
		t.Fatalf("SoftDelete: deleted=%v err=%v", deleted, err)
	}

	gotID, saved, err := st.SaveConversation(t.Context(), "user_1", "k1", "Thread", "#eng", "", savedMsgs("two"))
	if err != nil {
		t.Fatalf("resave: %v", err)
	}
	if saved {
		t.Fatal("a deleted copy must not be resurrected by the next save")
	}
	if gotID != id {
		t.Fatalf("expected the same derived id back, got %s", gotID)
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

	if _, saved, err := st.SaveConversation(t.Context(), "user_1", "k1", "Thread", "#eng", "", savedMsgs("x")); err != nil || saved {
		t.Fatalf("expected refusal, saved=%v err=%v", saved, err)
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

	id, saved, err := st.SaveConversation(t.Context(), "user_1", "k1", "Long", "", "", msgs)
	if err != nil || !saved {
		t.Fatalf("save: saved=%v err=%v", saved, err)
	}
	stored, _ := st.ListMessages(t.Context(), id)
	if len(stored) != MaxMessagesPerConversation {
		t.Fatalf("expected %d messages, got %d", MaxMessagesPerConversation, len(stored))
	}
	if stored[len(stored)-1].Content != "newest" {
		t.Fatal("truncation must drop the oldest turns, not the newest")
	}
}

func TestSaveConversation_RequiresUserAndKey(t *testing.T) {
	st := newTestStore(t)
	if _, _, err := st.SaveConversation(t.Context(), "", "k1", "t", "", "", nil); err == nil {
		t.Fatal("expected an error for an empty user id")
	}
	if _, _, err := st.SaveConversation(t.Context(), "user_1", "", "t", "", "", nil); err == nil {
		t.Fatal("expected an error for an empty idempotency key")
	}
}

func TestSaveConversation_UsesSourceTimestamps(t *testing.T) {
	st := newTestStore(t)
	when := time.Now().Add(-72 * time.Hour).Truncate(time.Millisecond)

	id, _, err := st.SaveConversation(t.Context(), "user_1", "k1", "t", "", "",
		[]SavedMessage{{Role: "user", Content: "old", Timestamp: when}})
	if err != nil {
		t.Fatalf("save: %v", err)
	}

	var createdMs int64
	if err := st.db.QueryRowContext(t.Context(),
		`SELECT created_at FROM messages WHERE conversation_id = ?`, id).Scan(&createdMs); err != nil {
		t.Fatalf("read created_at: %v", err)
	}
	if createdMs != when.UnixMilli() {
		t.Fatalf("expected the source timestamp %d, got %d", when.UnixMilli(), createdMs)
	}
}
