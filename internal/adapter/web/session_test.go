package web

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/astropods/messaging/internal/store"
	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
)

func TestFixedSessionManager_ReturnsConfiguredSessionForEveryRequest(t *testing.T) {
	mgr := NewFixedSessionManager(Session{
		UserID:   "user_local_dev",
		Username: "Dev",
		Email:    "dev@example.com",
	})

	// Two distinct requests with no auth headers should both resolve to the
	// configured session — the manager ignores request contents.
	for _, path := range []string{"/api/threads", "/static/app.js"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		got, err := mgr.ValidateRequest(context.Background(), req)
		if err != nil {
			t.Fatalf("ValidateRequest(%q) error: %v", path, err)
		}
		if got == nil {
			t.Fatalf("ValidateRequest(%q) returned nil session", path)
		}
		if got.UserID != "user_local_dev" {
			t.Errorf("UserID = %q, want %q", got.UserID, "user_local_dev")
		}
		if got.Username != "Dev" {
			t.Errorf("Username = %q, want %q", got.Username, "Dev")
		}
		if got.Email != "dev@example.com" {
			t.Errorf("Email = %q, want %q", got.Email, "dev@example.com")
		}
	}
}

func TestFixedSessionManager_ReturnsCopyNotSharedPointer(t *testing.T) {
	// Mutating one returned session must not bleed into subsequent calls —
	// handlers downstream may stamp request-specific metadata onto the
	// Session struct.
	mgr := NewFixedSessionManager(Session{UserID: "user_a"})

	first, err := mgr.ValidateRequest(context.Background(), httptest.NewRequest(http.MethodGet, "/", nil))
	if err != nil {
		t.Fatalf("first ValidateRequest: %v", err)
	}
	first.UserID = "mutated"

	second, err := mgr.ValidateRequest(context.Background(), httptest.NewRequest(http.MethodGet, "/", nil))
	if err != nil {
		t.Fatalf("second ValidateRequest: %v", err)
	}
	if second.UserID != "user_a" {
		t.Errorf("second.UserID = %q, want %q (mutation of first session leaked into the manager)", second.UserID, "user_a")
	}
}

func TestAstroServerSession_ForwardsTheSignedInUsersName(t *testing.T) {
	handlers := NewHandlers(NewConnectionManager(30*time.Second), NewAstroServerSessionManager(),
		store.NewThreadHistoryStore(100, 50, time.Hour), nil)
	var received *pb.Message
	handlers.SetMessageHandler(func(_ context.Context, msg *pb.Message) error {
		received = msg
		return nil
	})

	req := httptest.NewRequest(http.MethodPost, "/api/conversations/conv-1/messages",
		bytes.NewReader([]byte(`{"content":"hi"}`)))
	req.SetPathValue("id", "conv-1")
	req.Header.Set(HeaderUserID, "user_01J")
	req.Header.Set(HeaderUserName, "Sohum%20Dalal")
	w := httptest.NewRecorder()

	handlers.HandleSendMessage(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if received == nil {
		t.Fatal("expected the message forwarded to the agent")
	}
	if received.User.GetId() != "user_01J" {
		t.Errorf("User.Id = %q, want user_01J", received.User.GetId())
	}
	if received.User.GetUsername() != "Sohum Dalal" {
		t.Errorf("User.Username = %q, want the decoded Sohum Dalal", received.User.GetUsername())
	}
}

func TestAstroServerSession_DecodesTheName(t *testing.T) {
	cases := []struct {
		header string
		want   string
	}{
		{"Ada%20Lovelace", "Ada Lovelace"},
		{"Jos%C3%A9", "José"},
		{"C++", "C++"},
		{"%zz", ""},
		{"", ""},
	}
	sm := NewAstroServerSessionManager()
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set(HeaderUserID, "user_01J")
		req.Header.Set(HeaderUserName, tc.header)

		session, err := sm.ValidateRequest(context.Background(), req)
		if err != nil || session == nil {
			t.Fatalf("header %q: session = %v, err = %v; a bad name must not fail the session", tc.header, session, err)
		}
		if session.Username != tc.want {
			t.Errorf("header %q: Username = %q, want %q", tc.header, session.Username, tc.want)
		}
	}
}
