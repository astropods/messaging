package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
)

func newCancelHandlers() *Handlers {
	cm := NewConnectionManager(30 * time.Second)
	return NewHandlers(cm, &NoopSessionManager{}, nil, nil)
}

// No session -> 401, and no stop signal is forwarded to the agent.
func TestHandleCancel_NoSession_401(t *testing.T) {
	cm := NewConnectionManager(30 * time.Second)
	sm := NewHeaderSessionManager("X-User-ID", "", "") // missing header -> nil session
	h := NewHandlers(cm, sm, nil, nil)

	forwarded := false
	h.SetFeedbackHandler(func(context.Context, *pb.PlatformFeedback) error {
		forwarded = true
		return nil
	})

	req := httptest.NewRequest(http.MethodPost, "/api/conversations/c1/cancel", nil)
	req.SetPathValue("id", "c1")
	w := httptest.NewRecorder()
	h.HandleCancel(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d body=%q", w.Code, w.Body.String())
	}
	if forwarded {
		t.Errorf("unauthenticated cancel must not forward a stop signal")
	}
}

// Authenticated but no conversation id in the path -> 400.
func TestHandleCancel_MissingID_400(t *testing.T) {
	h := newCancelHandlers()

	req := httptest.NewRequest(http.MethodPost, "/api/conversations//cancel", nil)
	// No SetPathValue -> PathValue("id") == "".
	w := httptest.NewRecorder()
	h.HandleCancel(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d body=%q", w.Code, w.Body.String())
	}
}

// Happy path: 204 and the agent receives a StreamControl{STOP} for this
// conversation via the feedback handler.
func TestHandleCancel_ForwardsStreamControlStop(t *testing.T) {
	h := newCancelHandlers()

	var got *pb.PlatformFeedback
	h.SetFeedbackHandler(func(_ context.Context, fb *pb.PlatformFeedback) error {
		got = fb
		return nil
	})

	req := httptest.NewRequest(http.MethodPost, "/api/conversations/conv-123/cancel", nil)
	req.SetPathValue("id", "conv-123")
	w := httptest.NewRecorder()
	h.HandleCancel(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("want 204, got %d body=%q", w.Code, w.Body.String())
	}
	if got == nil {
		t.Fatalf("feedback handler was not invoked")
	}
	if got.ConversationId != "conv-123" {
		t.Errorf("conversation id: got %q, want %q", got.ConversationId, "conv-123")
	}
	sc := got.GetStreamControl()
	if sc == nil {
		t.Fatalf("expected StreamControl feedback, got %T", got.Feedback)
	}
	if sc.Action != pb.StreamControl_STOP {
		t.Errorf("action: got %v, want STOP", sc.Action)
	}
}

// A failing feedback handler must not change the response — the stop is
// best-effort and the sidecar has already finalized the turn.
func TestHandleCancel_FeedbackErrorStill204(t *testing.T) {
	h := newCancelHandlers()
	h.SetFeedbackHandler(func(context.Context, *pb.PlatformFeedback) error {
		return context.DeadlineExceeded
	})

	req := httptest.NewRequest(http.MethodPost, "/api/conversations/conv-err/cancel", nil)
	req.SetPathValue("id", "conv-err")
	w := httptest.NewRecorder()
	h.HandleCancel(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("want 204 despite feedback error, got %d body=%q", w.Code, w.Body.String())
	}
}
