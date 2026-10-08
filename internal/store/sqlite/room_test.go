package sqlite

import "testing"

func TestBindRoomBindsOnlyANewOwnedConversation(t *testing.T) {
	st := newTestStore(t)
	ctx := t.Context()
	for _, id := range []string{"fresh", "talked", "bound"} {
		if err := st.Upsert(ctx, id, "user-1", ""); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	if _, err := st.AppendMessage(ctx, "talked", "user-1", "user", "hi", ""); err != nil {
		t.Fatalf("seed message: %v", err)
	}
	if ok, err := st.BindRoom(ctx, "bound", "user-1", "room-a"); err != nil || !ok {
		t.Fatalf("seed binding = %v, %v", ok, err)
	}

	for _, tc := range []struct {
		name, conversation, user string
		want                     bool
	}{
		{"a new owned conversation", "fresh", "user-1", true},
		{"another user's conversation", "fresh", "user-2", false},
		{"a conversation that already has messages", "talked", "user-1", false},
		{"a conversation bound to another room", "bound", "user-1", false},
		{"a conversation that does not exist", "missing", "user-1", false},
	} {
		if ok, err := st.BindRoom(ctx, tc.conversation, tc.user, "room-b"); err != nil || ok != tc.want {
			t.Errorf("%s: bound = %v, %v; want %v", tc.name, ok, err, tc.want)
		}
	}
	if c, _ := st.Get(ctx, "bound"); c == nil || c.RoomID != "room-a" {
		t.Errorf("bound conversation = %+v, want it to keep room-a", c)
	}
}

func TestListKeepsRoomChatsAndPersonalChatsApart(t *testing.T) {
	st := newTestStore(t)
	ctx := t.Context()
	for id, room := range map[string]string{"personal": "", "in-a": "room-a", "in-b": "room-b"} {
		if err := st.Upsert(ctx, id, "user-1", ""); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
		if room != "" {
			if ok, err := st.BindRoom(ctx, id, "user-1", room); err != nil || !ok {
				t.Fatalf("bind %s: %v, %v", id, ok, err)
			}
		}
		if _, err := st.AppendMessage(ctx, id, "user-1", "user", "hi", ""); err != nil {
			t.Fatalf("seed message %s: %v", id, err)
		}
	}
	ids := func(convs []Conversation, err error) []string {
		t.Helper()
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		out := []string{}
		for _, c := range convs {
			out = append(out, c.ConversationID)
		}
		return out
	}
	if got := ids(st.ListByUser(ctx, "user-1")); len(got) != 1 || got[0] != "personal" {
		t.Errorf("ListByUser = %v, want only the personal chat", got)
	}
	if got := ids(st.ListByRoom(ctx, "user-1", "room-a")); len(got) != 1 || got[0] != "in-a" {
		t.Errorf("ListByRoom(room-a) = %v, want only room-a's chat", got)
	}
}
