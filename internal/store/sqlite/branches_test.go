package sqlite

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

type chat struct {
	t    *testing.T
	st   *Store
	conv string
}

func newChat(t *testing.T) *chat {
	t.Helper()
	st := newTestStore(t)
	if _, err := st.EnsureForSend(context.Background(), "conv", "owner", "t"); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	return &chat{t: t, st: st, conv: "conv"}
}

func (c *chat) send(content, editOf string) UserTurn {
	c.t.Helper()
	turn, err := c.st.AppendUserTurn(context.Background(), c.conv, "owner", TurnInput{Content: content, EditOf: editOf})
	if err != nil {
		c.t.Fatalf("append user turn %q: %v", content, err)
	}
	return turn
}

func (c *chat) reply(content string) string {
	c.t.Helper()
	id, err := c.st.UpsertAssistantProgress(context.Background(), c.conv, content, "")
	if err != nil {
		c.t.Fatalf("assistant reply %q: %v", content, err)
	}
	return id
}

func (c *chat) finish(content string) string {
	c.t.Helper()
	id, err := c.st.FinishAssistantReply(context.Background(), c.conv, content, "")
	if err != nil {
		c.t.Fatalf("assistant reply %q: %v", content, err)
	}
	return id
}

func (c *chat) turn(content, answer string) UserTurn {
	c.t.Helper()
	u := c.send(content, "")
	c.finish(answer)
	return u
}

func (c *chat) page() []Message {
	c.t.Helper()
	msgs, hasMore, _, _, err := c.st.PageMessages(context.Background(), c.conv, 100, 0)
	if err != nil || hasMore {
		c.t.Fatalf("page messages: hasMore=%v err=%v, want the whole branch on one page", hasMore, err)
	}
	return msgs
}

func branchContents(msgs []Message) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.Content)
	}
	return out
}

func TestAnEditReplacesTheMessageOnANewBranch(t *testing.T) {
	c := newChat(t)
	c.turn("q1", "a1")
	q2 := c.turn("q2", "a2")

	edited := c.send("q2 edited", q2.ID)
	c.finish("a2 edited")

	msgs := c.page()
	if got, want := branchContents(msgs), []string{"q1", "a1", "q2 edited", "a2 edited"}; !slices.Equal(got, want) {
		t.Fatalf("active branch = %v, want %v", got, want)
	}
	if got, want := msgs[2].Branches, []string{q2.ID, edited.ID}; !slices.Equal(got, want) {
		t.Errorf("edited message branches = %v, want the original then the edit %v", got, want)
	}
	for _, i := range []int{0, 1, 3} {
		if msgs[i].Branches != nil {
			t.Errorf("message %q has branches %v, want none", msgs[i].Content, msgs[i].Branches)
		}
	}
}

func TestAnEditCarriesTheBranchBeforeItAsHistory(t *testing.T) {
	c := newChat(t)
	c.turn("q1", "a1")
	q2 := c.turn("q2", "a2")

	edited := c.send("q2 edited", q2.ID)

	if edited.History == nil {
		t.Fatal("an edit must carry history: the agent's memory still holds q2 and a2")
	}
	if got, want := branchContents(edited.History.Messages), []string{"q1", "a1"}; !slices.Equal(got, want) {
		t.Errorf("history = %v, want the turns before the edited message %v", got, want)
	}
	if !edited.History.Complete {
		t.Error("history under the size budget must be complete")
	}
}

func TestAnEditOfTheFirstMessageCarriesEmptyHistory(t *testing.T) {
	c := newChat(t)
	q1 := c.turn("q1", "a1")

	edited := c.send("q1 edited", q1.ID)

	if edited.History == nil || len(edited.History.Messages) != 0 {
		t.Fatalf("history = %+v, want present and empty so the agent clears its memory", edited.History)
	}
	msgs := c.page()
	if got, want := branchContents(msgs), []string{"q1 edited"}; !slices.Equal(got, want) {
		t.Fatalf("active branch = %v, want %v", got, want)
	}
	if got, want := msgs[0].Branches, []string{q1.ID, edited.ID}; !slices.Equal(got, want) {
		t.Errorf("root branches = %v, want %v", got, want)
	}
}

func TestAnOrdinaryTurnCarriesNoHistory(t *testing.T) {
	c := newChat(t)
	first := c.send("q1", "")
	c.finish("a1")

	second := c.send("q2", "")

	if first.History != nil || second.History != nil {
		t.Fatalf("history = %+v then %+v, want none on turns that continue what the agent holds", first.History, second.History)
	}
}

func TestATurnTheAgentNeverReceivedIsReplayedOnTheNextSend(t *testing.T) {
	c := newChat(t)
	c.send("q1", "")
	if _, err := c.st.FinalizeTerminal(context.Background(), c.conv, ""); err != nil {
		t.Fatalf("finalize: %v", err)
	}

	next := c.send("q2", "")

	if next.History == nil {
		t.Fatal("the agent never received q1, so the next send must carry it")
	}
	if got, want := branchContents(next.History.Messages), []string{"q1"}; !slices.Equal(got, want) {
		t.Errorf("history = %v, want %v (the empty finalized reply is left out)", got, want)
	}
}

func TestSwitchBranchShowsTheNewestMessageUnderTheAlternative(t *testing.T) {
	c := newChat(t)
	c.turn("q1", "a1")
	q2 := c.turn("q2", "a2")
	c.turn("q3", "a3")
	c.send("q2 edited", q2.ID)
	c.finish("a2 edited")

	ok, err := c.st.SwitchBranch(context.Background(), c.conv, "owner", q2.ID)
	if err != nil || !ok {
		t.Fatalf("switch branch: ok=%v err=%v", ok, err)
	}
	if got, want := branchContents(c.page()), []string{"q1", "a1", "q2", "a2", "q3", "a3"}; !slices.Equal(got, want) {
		t.Fatalf("active branch after switch = %v, want the original branch to its newest message %v", got, want)
	}

	next := c.send("q4", "")
	if next.History == nil {
		t.Fatal("a send after a branch switch must carry history")
	}
	if got, want := branchContents(next.History.Messages), []string{"q1", "a1", "q2", "a2", "q3", "a3"}; !slices.Equal(got, want) {
		t.Errorf("history = %v, want %v", got, want)
	}
	if got := branchContents(c.page()); got[len(got)-1] != "q4" {
		t.Errorf("active branch = %v, want it to end at the new message", got)
	}
}

func TestSwitchBranchRejectsAForeignConversationOrUnknownMessage(t *testing.T) {
	c := newChat(t)
	q1 := c.turn("q1", "a1")

	for name, tc := range map[string]struct{ user, message string }{
		"foreign user":    {"intruder", q1.ID},
		"unknown message": {"owner", "missing"},
	} {
		ok, err := c.st.SwitchBranch(context.Background(), c.conv, tc.user, tc.message)
		if err != nil || ok {
			t.Errorf("%s: ok=%v err=%v, want not found", name, ok, err)
		}
	}
}

func TestEditOfAnAssistantOrUnknownMessageIsRejected(t *testing.T) {
	c := newChat(t)
	c.turn("q1", "a1")
	assistant := c.page()[1].ID

	for _, editOf := range []string{assistant, "missing"} {
		_, err := c.st.AppendUserTurn(context.Background(), c.conv, "owner", TurnInput{Content: "x", EditOf: editOf})
		if !errors.Is(err, ErrInvalidEdit) {
			t.Errorf("edit_of %q: err = %v, want ErrInvalidEdit", editOf, err)
		}
	}
	if got := len(c.page()); got != 2 {
		t.Errorf("a rejected edit wrote a row: %d messages, want 2", got)
	}
}

func TestHistoryKeepsTheNewestTurnsWithinTheBudget(t *testing.T) {
	c := newChat(t)
	big := strings.Repeat("x", MaxMessageContentRunes)
	for _, q := range []string{"q1", "q2", "q3", "q4", "q5"} {
		c.turn(q, big)
	}
	last := c.turn("q6", "a6")

	edited := c.send("q6 edited", last.ID)

	if edited.History == nil || edited.History.Complete {
		t.Fatalf("history complete=%v, want incomplete: five full replies exceed maxHistoryBytes", edited.History != nil && edited.History.Complete)
	}
	got := edited.History.Messages
	if len(got) != 8 || got[0].Content != "q2" {
		t.Errorf("history kept %d messages starting at %q, want the newest 8 starting at q2", len(got), got[0].Content)
	}
}

func TestPageMessagesPagesAlongTheActiveBranch(t *testing.T) {
	c := newChat(t)
	c.turn("q1", "a1")
	q2 := c.turn("q2", "a2")
	c.send("q2 edited", q2.ID)
	c.finish("a2 edited")

	tail, hasMore, oldest, _, err := c.st.PageMessages(context.Background(), c.conv, 2, 0)
	if err != nil {
		t.Fatalf("page: %v", err)
	}
	if got, want := branchContents(tail), []string{"q2 edited", "a2 edited"}; !slices.Equal(got, want) || !hasMore {
		t.Fatalf("tail = %v hasMore=%v, want %v and more", got, hasMore, want)
	}
	older, hasMore, _, _, err := c.st.PageMessages(context.Background(), c.conv, 2, oldest)
	if err != nil {
		t.Fatalf("older page: %v", err)
	}
	if got, want := branchContents(older), []string{"q1", "a1"}; !slices.Equal(got, want) || hasMore {
		t.Errorf("older page = %v hasMore=%v, want %v and no more (the abandoned branch is not on it)", got, hasMore, want)
	}
}

func TestRowsWithoutParentsReadAsOneBranch(t *testing.T) {
	c := newChat(t)
	for _, content := range []string{"q1", "a1", "q2"} {
		role := "user"
		if content == "a1" {
			role = "assistant"
		}
		if _, err := c.st.AppendMessage(context.Background(), c.conv, "owner", role, content, ""); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	msgs := c.page()
	if got, want := branchContents(msgs), []string{"q1", "a1", "q2"}; !slices.Equal(got, want) {
		t.Fatalf("active branch = %v, want %v", got, want)
	}
	for _, m := range msgs {
		if m.Branches != nil {
			t.Errorf("linear message %q has branches %v", m.Content, m.Branches)
		}
	}
}

func TestAnAppendSaveShowsTheAppendedMessages(t *testing.T) {
	st := newTestStore(t)
	id, _ := save(t, st, SaveRequest{Messages: savedMsgs("s1", "s2")})
	c := &chat{t: t, st: st, conv: id}
	q := c.send("reply", "")
	c.finish("answer")
	c.send("reply edited", q.ID)
	c.finish("answer edited")
	if ok, err := st.SwitchBranch(t.Context(), id, "user_1", q.ID); err != nil || !ok {
		t.Fatalf("switch: ok=%v err=%v", ok, err)
	}

	save(t, st, SaveRequest{Messages: savedMsgs("s3"), OnConflict: OnConflictAppend})

	got := branchContents(c.page())
	if got[len(got)-1] != "s3" {
		t.Errorf("active branch = %v, want it to end at the appended message", got)
	}
}

func TestAnEditOfATurnTheAgentErroredOnCarriesHistory(t *testing.T) {
	c := newChat(t)
	c.turn("q1", "a1")
	q2 := c.send("q2", "")
	if _, err := c.st.FinalizeTerminal(context.Background(), c.conv, ""); err != nil {
		t.Fatalf("finalize: %v", err)
	}

	edited := c.send("q2 edited", q2.ID)

	if edited.History == nil {
		t.Fatal("the agent may still hold q2, so an edit of it must carry history")
	}
	if got, want := branchContents(edited.History.Messages), []string{"q1", "a1"}; !slices.Equal(got, want) {
		t.Errorf("history = %v, want %v", got, want)
	}
}

func TestASendAfterAPartialReplyThatErroredCarriesHistory(t *testing.T) {
	c := newChat(t)
	c.turn("q1", "a1")
	c.send("q2", "")
	c.reply("partial")
	// FinalizeTerminal is a no-op here because an assistant row exists.
	if _, err := c.st.FinalizeTerminal(context.Background(), c.conv, "partial"); err != nil {
		t.Fatalf("finalize: %v", err)
	}

	next := c.send("q3", "")

	if next.History == nil {
		t.Fatal("the agent errored mid-reply and may not hold the partial the user sees, so the next send must carry history")
	}
}

func TestAReplaceSaveMakesTheNextSendCarryTheSavedTranscript(t *testing.T) {
	st := newTestStore(t)
	id, _ := save(t, st, SaveRequest{Messages: savedMsgs("s1", "s2")})
	c := &chat{t: t, st: st, conv: id}
	c.turn("q", "a")

	save(t, st, SaveRequest{Messages: savedMsgs("s3", "s4"), OnConflict: OnConflictReplace})
	next := c.send("q2", "")

	if next.History == nil {
		t.Fatal("REPLACE discarded q and a, which the agent still holds, so the next send must carry history")
	}
	if got, want := branchContents(next.History.Messages), []string{"s3", "s4"}; !slices.Equal(got, want) {
		t.Errorf("history = %v, want %v", got, want)
	}
}

func TestASendAnchoredOnAnotherBranchIsRefused(t *testing.T) {
	c := newChat(t)
	c.turn("q1", "a1")
	q2 := c.turn("q2", "a2")
	old := c.page()[3].ID // a2, the head another tab still shows
	c.send("q2 edited", q2.ID)
	newest := c.finish("a2 edited")

	_, err := c.st.AppendUserTurn(context.Background(), c.conv, "owner", TurnInput{Content: "q3", Anchor: old})
	if !errors.Is(err, ErrStaleBranch) {
		t.Fatalf("err = %v, want ErrStaleBranch for a send anchored on the old branch", err)
	}
	if _, err := c.st.AppendUserTurn(context.Background(), c.conv, "owner", TurnInput{Content: "q3", Anchor: newest}); err != nil {
		t.Fatalf("a send anchored on the active head: %v", err)
	}
}

// An older image appends without resetting head_id.
func TestAnAppendByAnOlderImageMakesASwitchedHeadStale(t *testing.T) {
	c := newChat(t)
	c.turn("q1", "a1")
	q2 := c.turn("q2", "a2")
	c.send("q2 edited", q2.ID)
	c.finish("a2 edited")
	if ok, err := c.st.SwitchBranch(context.Background(), c.conv, "owner", q2.ID); err != nil || !ok {
		t.Fatalf("switch: ok=%v err=%v", ok, err)
	}
	if _, err := c.st.db.Exec(`
		INSERT INTO messages (id, conversation_id, user_id, role, content, seq, created_at)
		VALUES ('old-image', ?, 'owner', 'user', 'q3', 99, 0)`, c.conv); err != nil {
		t.Fatalf("simulate an older image's append: %v", err)
	}

	got := branchContents(c.page())
	if got[len(got)-1] != "q3" {
		t.Errorf("active branch = %v, want it to follow the newest message", got)
	}
}

func TestHistoryNamesTheFilesOfAUserTurnWithoutText(t *testing.T) {
	c := newChat(t)
	if _, err := c.st.AppendUserTurn(context.Background(), c.conv, "owner", TurnInput{
		AttachmentsJSON: `[{"key":"k1","name":"report.pdf"},{"key":"k2","name":"shot.png"}]`,
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	c.finish("a1")
	q2 := c.turn("q2", "a2")

	edited := c.send("q2 edited", q2.ID)

	if got, want := branchContents(edited.History.Messages), []string{"[Attached: report.pdf, shot.png]", "a1"}; !slices.Equal(got, want) {
		t.Errorf("history = %v, want %v", got, want)
	}
}

func TestASendAfterAStoppedTurnCarriesHistory(t *testing.T) {
	c := newChat(t)
	c.turn("q1", "a1")
	c.send("q2", "")
	if _, err := c.st.FinalizeStopped(context.Background(), c.conv, "owner", "partial"); err != nil {
		t.Fatalf("stop: %v", err)
	}

	if next := c.send("q3", ""); next.History == nil {
		t.Fatal("an adapter may drop a stopped turn, so the next send must carry history")
	}
}

func TestTheFirstSendInASavedCopyCarriesTheTranscript(t *testing.T) {
	st := newTestStore(t)
	id, _ := save(t, st, SaveRequest{Messages: savedMsgs("s1", "s2")})
	c := &chat{t: t, st: st, conv: id}

	first := c.send("q", "")

	if first.History == nil {
		t.Fatal("the agent never held the copied turns under this conversation, so the first send must carry them")
	}
	if got, want := branchContents(first.History.Messages), []string{"s1", "s2"}; !slices.Equal(got, want) {
		t.Errorf("history = %v, want %v", got, want)
	}
}
