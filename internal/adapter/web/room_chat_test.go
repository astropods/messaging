package web

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
)

func roomGrantToken(t *testing.T, room string) string {
	t.Helper()
	return roomGrantTokenUntil(t, room, time.Now().Add(time.Minute))
}

func roomGrantTokenUntil(t *testing.T, room string, expires time.Time) string {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.EdDSA, Key: key}, nil)
	if err != nil {
		t.Fatal(err)
	}
	token, err := jwt.Signed(signer).Claims(map[string]any{"scope": room, "message_id": "chat-1", "exp": expires.Unix()}).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return token
}

type roomChatHarness struct {
	t         *testing.T
	h         *Handlers
	adapter   *WebAdapter
	forwarded []*pb.Message
}

func newRoomChatHarness(t *testing.T) *roomChatHarness {
	t.Helper()
	h, _ := newChatTitleHandlers(t)
	r := &roomChatHarness{t: t, h: h, adapter: &WebAdapter{handlers: h, roomAPIURL: "https://astro.test"}}
	h.msgHandler = func(_ context.Context, m *pb.Message) error {
		r.forwarded = append(r.forwarded, m)
		return nil
	}
	return r
}

func roomHeaders(req *http.Request, room, grant string) *http.Request {
	if room != "" {
		req.Header.Set(HeaderRoomID, room)
		req.Header.Set(HeaderRoomGrant, grant)
	}
	return req
}

func (r *roomChatHarness) create(room, grant string) string {
	r.t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/conversations", strings.NewReader(`{}`))
	req.Header.Set("X-User-ID", "user-1")
	w := httptest.NewRecorder()
	r.h.HandleCreateConversation(w, roomHeaders(req, room, grant))
	if w.Code != http.StatusCreated {
		r.t.Fatalf("create = %d %s", w.Code, w.Body.String())
	}
	var out CreateConversationResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		r.t.Fatalf("decode create: %v", err)
	}
	return out.ConversationID
}

func (r *roomChatHarness) send(conversationID, room, grant string) int {
	r.t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/conversations/"+conversationID+"/messages", strings.NewReader(`{"content":"hi"}`))
	req.SetPathValue("id", conversationID)
	req.Header.Set("X-User-ID", "user-1")
	w := httptest.NewRecorder()
	r.h.HandleSendMessage(w, roomHeaders(req, room, grant))
	if r.h.turns != nil {
		r.h.turns.endTurn(conversationID)
	}
	return w.Code
}

func TestRoomChatCarriesItsRoomToTheAgentAndServesItsGrant(t *testing.T) {
	r := newRoomChatHarness(t)
	grant := roomGrantToken(t, "room-a")
	conv := r.create("room-a", grant)

	if code := r.send(conv, "room-a", grant); code != http.StatusOK {
		t.Fatalf("send in room-a = %d, want 200", code)
	}
	if len(r.forwarded) != 1 || r.forwarded[0].GetPlatformContext().GetPlatformData()["mesh_scope"] != "room-a" {
		t.Fatalf("forwarded = %+v, want one turn with mesh_scope room-a", r.forwarded)
	}
	got, ok := r.adapter.RoomGrant(conv)
	if !ok || got.RoomID != "room-a" || got.Grant != grant || got.APIURL != "https://astro.test" {
		t.Fatalf("RoomGrant = %+v, %v; want room-a's grant with the room API URL", got, ok)
	}
	if _, ok := r.adapter.RoomGrant("another-chat"); ok {
		t.Fatal("RoomGrant answered for a conversation that is not a room chat")
	}
}

func TestRoomChatKeepsTheGrantFromTheLatestSend(t *testing.T) {
	r := newRoomChatHarness(t)
	conv := r.create("room-a", roomGrantToken(t, "room-a"))
	fresh := roomGrantToken(t, "room-a")
	if code := r.send(conv, "room-a", fresh); code != http.StatusOK {
		t.Fatalf("send = %d", code)
	}
	if got, _ := r.adapter.RoomGrant(conv); got.Grant != fresh {
		t.Fatal("RoomGrant kept the create's grant, want the latest send's")
	}
}

func TestAFirstSendWithARoomBindsANewConversation(t *testing.T) {
	r := newRoomChatHarness(t)
	if code := r.send("new-chat", "room-a", roomGrantToken(t, "room-a")); code != http.StatusOK {
		t.Fatalf("first send = %d, want 200", code)
	}
	if code := r.send("new-chat", "", ""); code != http.StatusConflict {
		t.Fatalf("send without the room after binding = %d, want 409", code)
	}
}

func TestRoomChatRefusesASendFromAnotherRoomOrNone(t *testing.T) {
	r := newRoomChatHarness(t)
	roomChat := r.create("room-a", roomGrantToken(t, "room-a"))
	personal := r.create("", "")
	if code := r.send(personal, "", ""); code != http.StatusOK {
		t.Fatalf("personal send = %d, want 200", code)
	}

	for _, tc := range []struct {
		name, conversation, room string
	}{
		{"a room chat sent from another room", roomChat, "room-b"},
		{"a room chat sent with no room", roomChat, ""},
		{"a personal chat sent from a room", personal, "room-a"},
	} {
		before := len(r.forwarded)
		if code := r.send(tc.conversation, tc.room, roomGrantToken(t, tc.room)); code != http.StatusConflict {
			t.Errorf("%s: status = %d, want 409", tc.name, code)
		}
		if len(r.forwarded) != before {
			t.Errorf("%s: the turn reached the agent, want it stopped", tc.name)
		}
	}
}

func TestChatListFiltersByRoom(t *testing.T) {
	r := newRoomChatHarness(t)
	roomChat := r.create("room-a", roomGrantToken(t, "room-a"))
	personal := r.create("", "")
	for conv, room := range map[string]string{roomChat: "room-a", personal: ""} {
		if code := r.send(conv, room, roomGrantToken(t, room)); code != http.StatusOK {
			t.Fatalf("send = %d", code)
		}
	}
	list := func(query string) []string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/chat/conversations"+query, nil)
		req.Header.Set("X-User-ID", "user-1")
		w := httptest.NewRecorder()
		r.h.HandleListChatConversations(w, req)
		var out listChatConversationsResponse
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode list: %v", err)
		}
		ids := []string{}
		for _, c := range out.Conversations {
			ids = append(ids, c.ConversationID)
		}
		return ids
	}
	if got := list("?room_id=room-a"); len(got) != 1 || got[0] != roomChat {
		t.Errorf("room-a list = %v, want only its room chat", got)
	}
	if got := list(""); len(got) != 1 || got[0] != personal {
		t.Errorf("unfiltered list = %v, want only the personal chat", got)
	}
}

func TestRoomChatsDropGrantsThatHaveExpired(t *testing.T) {
	rooms := newRoomChats()
	now := time.Now()
	rooms.set("stale", "room-a", roomGrantTokenUntil(t, "room-a", now.Add(-time.Second)), now)
	rooms.set("live", "room-a", roomGrantTokenUntil(t, "room-a", now.Add(time.Minute)), now)
	rooms.set("latest", "room-b", roomGrantTokenUntil(t, "room-b", now.Add(time.Minute)), now)

	if _, ok := rooms.get("stale"); ok {
		t.Error("an expired grant is still held after a later set")
	}
	for _, id := range []string{"live", "latest"} {
		if _, ok := rooms.get(id); !ok {
			t.Errorf("live grant %q was dropped", id)
		}
	}
	if n := rooms.len(); n != 2 {
		t.Fatalf("held %d room chats, want 2 so the map does not grow with every chat", n)
	}
}
