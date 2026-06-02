package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFixedSessionManager_ReturnsConfiguredSessionForEveryRequest(t *testing.T) {
	mgr := NewFixedSessionManager(Session{
		UserID:   "user_local_dev",
		Username: "Dev",
		Email:    "dev@example.com",
	})

	// Two distinct requests with no auth headers should both resolve to the
	// configured session — the manager ignores request contents.
	for _, path := range []string{"/api/threads", "/assets/main.js"} {
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
