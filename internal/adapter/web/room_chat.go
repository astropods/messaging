package web

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/astropods/messaging/internal/roomgrant"
)

// astro-server sets these on a room chat request; its proxy never forwards them from a client.
const (
	HeaderRoomID    = "X-Astro-Room-Id"
	HeaderRoomGrant = "X-Astro-Room-Grant"
)

type roomChat struct {
	roomID  string
	grant   string
	expires time.Time
}

type roomChats struct {
	mu   sync.Mutex
	byID map[string]roomChat
}

func newRoomChats() *roomChats {
	return &roomChats{byID: map[string]roomChat{}}
}

func (r *roomChats) get(conversationID string) (roomChat, bool) {
	if r == nil {
		return roomChat{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.byID[conversationID]
	return c, ok
}

// set keeps the conversation's latest grant and drops entries whose grant has expired.
func (r *roomChats) set(conversationID, roomID, grant string, now time.Time) {
	if r == nil {
		return
	}
	expires, _ := roomgrant.Expiry(grant)
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, c := range r.byID {
		if !now.Before(c.expires) {
			delete(r.byID, id)
		}
	}
	r.byID[conversationID] = roomChat{roomID: roomID, grant: grant, expires: expires}
}

func (r *roomChats) len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.byID)
}

func WithRoomAPIURL(url string) WebAdapterOption {
	return func(a *WebAdapter) {
		a.roomAPIURL = url
	}
}

func (a *WebAdapter) RoomGrant(conversationID string) (roomgrant.Grant, bool) {
	if a.handlers == nil {
		return roomgrant.Grant{}, false
	}
	c, ok := a.handlers.rooms.get(conversationID)
	if !ok {
		return roomgrant.Grant{}, false
	}
	return roomgrant.Live(c.roomID, c.grant, a.roomAPIURL, time.Now())
}

// boundRoom reports the room a conversation is bound to, and whether the
// conversation exists yet, from memory first and then the chat store.
func (h *Handlers) boundRoom(ctx context.Context, conversationID string) (string, bool, error) {
	if c, ok := h.rooms.get(conversationID); ok {
		return c.roomID, true, nil
	}
	if h.chatStore == nil {
		return "", false, nil
	}
	conv, err := h.chatStore.Get(ctx, conversationID)
	if err != nil || conv == nil {
		return "", false, err
	}
	return conv.RoomID, true, nil
}

func (h *Handlers) bindRoom(ctx context.Context, conversationID, userID, roomID string) error {
	if h.chatStore == nil {
		return nil
	}
	_, err := h.chatStore.BindRoom(ctx, conversationID, userID, roomID)
	return err
}

func writeRoomMismatch(w http.ResponseWriter) {
	writeJSON(w, http.StatusConflict, map[string]string{
		"error":             "room_mismatch",
		"error_description": "this conversation belongs to a different room, or to none",
	})
}
