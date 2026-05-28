package slack

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/astropods/messaging/internal/langfuse"
	"github.com/astropods/messaging/internal/store"
)

// Tests for the Langfuse-score branch of the feedback flow. The PlatformFeedback
// forwarding path is covered by adapter_test.go; these tests focus on what's
// new: the trace_id storage on END + score submission on feedback click.

func newScoreTestAdapter(t *testing.T, scoreSrv *httptest.Server) *SlackAdapter {
	t.Helper()
	a := &SlackAdapter{
		contentBuffers:    make(map[string]string),
		pendingTraceIDs:   make(map[string]string),
		feedbackStore:     store.NewMemoryFeedbackStore(0),
	}
	if scoreSrv != nil {
		a.langfuseClient = langfuse.New(scoreSrv.URL, "pk", "sk")
	} else {
		a.langfuseClient = langfuse.New("", "", "") // disabled
	}
	return a
}

func TestBindPendingTraceID_MovesPendingToStore(t *testing.T) {
	a := newScoreTestAdapter(t, nil)
	a.pendingTraceIDs["conv-1"] = "trace-abc"

	a.bindPendingTraceID(context.Background(), "conv-1", "msg-ts-1")

	if len(a.pendingTraceIDs) != 0 {
		t.Fatalf("pending map should be drained; got %d entries", len(a.pendingTraceIDs))
	}
	got, _ := a.feedbackStore.GetTraceID(context.Background(), "msg-ts-1")
	if got != "trace-abc" {
		t.Fatalf("feedbackStore.GetTraceID = %q; want trace-abc", got)
	}
}

func TestBindPendingTraceID_NoPending_NoOp(t *testing.T) {
	a := newScoreTestAdapter(t, nil)
	a.bindPendingTraceID(context.Background(), "conv-1", "msg-ts-1")
	// No store entry created
	got, _ := a.feedbackStore.GetTraceID(context.Background(), "msg-ts-1")
	if got != "" {
		t.Fatalf("expected empty store; got %q", got)
	}
}

func TestSubmitLangfuseScore_NoTraceID_Skipped(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	a := newScoreTestAdapter(t, srv)
	// No trace_id ever stored — submitLangfuseScore should silently skip.
	a.submitLangfuseScore(context.Background(), "msg-ts-1", "user-feedback", "id-1", 1, "")

	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("Langfuse POST count = %d; want 0 (no trace_id mapping)", got)
	}
}

func TestSubmitLangfuseScore_HappyPath_PostsScore(t *testing.T) {
	var gotBody langfuse.ScoreRequest
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	a := newScoreTestAdapter(t, srv)
	_ = a.feedbackStore.SetTraceID(context.Background(), "msg-ts-1", "trace-abc")

	a.submitLangfuseScore(context.Background(), "msg-ts-1", "user-feedback", "msg-ts-1-user-feedback", 0, "")

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("Langfuse POST count = %d; want 1", got)
	}
	if gotBody.TraceID != "trace-abc" || gotBody.Name != "user-feedback" || gotBody.Value != 0 || gotBody.ID != "msg-ts-1-user-feedback" {
		t.Fatalf("body mismatch: %+v", gotBody)
	}
}

func TestSubmitLangfuseScore_DisabledClient_NoOp(t *testing.T) {
	a := newScoreTestAdapter(t, nil) // no scoreSrv → disabled client
	_ = a.feedbackStore.SetTraceID(context.Background(), "msg-ts-1", "trace-abc")

	// Must not panic, must not error.
	a.submitLangfuseScore(context.Background(), "msg-ts-1", "user-feedback", "id-1", 1, "")
}

func TestSubmitLangfuseScore_CommentPayload(t *testing.T) {
	var gotBody langfuse.ScoreRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	a := newScoreTestAdapter(t, srv)
	_ = a.feedbackStore.SetTraceID(context.Background(), "msg-ts-2", "trace-xyz")

	a.submitLangfuseScore(context.Background(), "msg-ts-2", "user-comment", "msg-ts-2-user-comment", 0.5, "great answer")

	if gotBody.Name != "user-comment" || gotBody.Comment != "great answer" || gotBody.Value != 0.5 {
		t.Fatalf("body mismatch: %+v", gotBody)
	}
}
