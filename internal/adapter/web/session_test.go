package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOrderedSessionManagers_FirstWinner(t *testing.T) {
	t.Parallel()
	h := OrderedSessionManagers{
		NewHeaderSessionManager("X-OIDC", "", ""),
		AnonymousWebFallbackSessionManager{},
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-OIDC", "user_abc")
	got, err := h.ValidateRequest(context.Background(), req)
	if err != nil {
		t.Fatalf("ValidateRequest: %v", err)
	}
	if got == nil || got.UserID != "user_abc" {
		t.Fatalf("want user_abc session, got %+v", got)
	}
}

func TestOrderedSessionManagers_FallbackAnonymous(t *testing.T) {
	t.Parallel()
	h := OrderedSessionManagers{
		NewHeaderSessionManager("X-OIDC", "", ""),
		AnonymousWebFallbackSessionManager{},
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	got, err := h.ValidateRequest(context.Background(), req)
	if err != nil {
		t.Fatalf("ValidateRequest: %v", err)
	}
	if got == nil {
		t.Fatal("expected synthetic session")
	}
	if got.UserID != "" {
		t.Fatalf("expected empty principal UserID, got %q", got.UserID)
	}
	if got.Username != "anonymous" {
		t.Fatalf("username: got %q", got.Username)
	}
}

func TestOrderedSessionManagers_SkipsNilDelegates(t *testing.T) {
	t.Parallel()
	h := OrderedSessionManagers{nil, AnonymousWebFallbackSessionManager{}}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	got, err := h.ValidateRequest(context.Background(), req)
	if err != nil {
		t.Fatalf("ValidateRequest: %v", err)
	}
	if got == nil {
		t.Fatal("expected fallback")
	}
}
