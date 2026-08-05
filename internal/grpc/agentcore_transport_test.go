package grpc

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/astropods/messaging/internal/store"
	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
)

// newTestServer builds a Server with in-memory stores and a registered mock
// adapter, returning both so tests can assert what got routed back.
func newTestServer(t *testing.T, platform string) (*Server, *mockAdapter) {
	t.Helper()
	threadStore := store.NewThreadHistoryStore(100, 50, time.Hour)
	convStore := store.NewMemoryStore()
	srv := NewServer(":0", threadStore, convStore, nil)
	mock := newMockAdapter(platform)
	srv.RegisterAdapter(platform, mock)
	return srv, mock
}

func testMessage() *pb.Message {
	return &pb.Message{
		Id:             "msg-1",
		ConversationId: "conv-1",
		Platform:       "web",
		Content:        "hello there",
		User:           &pb.User{Id: "u1"},
	}
}

// sseHandler returns an httptest handler that emits the given raw SSE body for
// POST /invocations and 404s elsewhere.
func sseHandler(t *testing.T, body string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/invocations" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, body)
	}
}

func TestAgentCoreTransport_RoutesStartDeltaEnd(t *testing.T) {
	srv, mock := newTestServer(t, "web")

	body := "data: {\"type\":\"START\"}\n\n" +
		"data: {\"type\":\"DELTA\",\"content\":\"Echo: hello there\"}\n\n" +
		"data: {\"type\":\"DELTA\",\"content\":\"Here's a joke: x\"}\n\n" +
		"data: {\"type\":\"END\"}\n\n"
	ts := httptest.NewServer(sseHandler(t, body))
	defer ts.Close()

	tx := NewAgentCoreTransport(srv, NewHTTPInvoker(ts.URL))
	if err := tx.HandleIncomingMessage(context.Background(), testMessage()); err != nil {
		t.Fatalf("HandleIncomingMessage: %v", err)
	}

	got := mock.getResponseCount()
	if got != 4 {
		t.Fatalf("expected 4 routed responses (START, 2×DELTA, END), got %d", got)
	}

	// First = START content chunk.
	first := mock.responses[0].GetContent()
	if first == nil || first.Type != pb.ContentChunk_START {
		t.Fatalf("expected START content chunk first, got %+v", mock.responses[0])
	}
	// Deltas carry the text.
	if d := mock.responses[1].GetContent(); d == nil || d.Content != "Echo: hello there" {
		t.Fatalf("expected first DELTA text, got %+v", mock.responses[1])
	}
	// Last = END.
	last := mock.getLastResponse().GetContent()
	if last == nil || last.Type != pb.ContentChunk_END {
		t.Fatalf("expected END content chunk last, got %+v", mock.getLastResponse())
	}

	// conversation_id + response_id preserved from the incoming message.
	if mock.responses[0].ConversationId != "conv-1" || mock.responses[0].ResponseId != "msg-1" {
		t.Fatalf("ids not preserved: %+v", mock.responses[0])
	}
}

func TestAgentCoreTransport_ErrorEventBecomesErrorResponse(t *testing.T) {
	srv, mock := newTestServer(t, "web")

	body := "data: {\"type\":\"START\"}\n\n" +
		"data: {\"type\":\"ERROR\",\"code\":\"AGENT_ERROR\",\"message\":\"boom\"}\n\n"
	ts := httptest.NewServer(sseHandler(t, body))
	defer ts.Close()

	tx := NewAgentCoreTransport(srv, NewHTTPInvoker(ts.URL))
	if err := tx.HandleIncomingMessage(context.Background(), testMessage()); err != nil {
		t.Fatalf("HandleIncomingMessage: %v", err)
	}

	last := mock.getLastResponse()
	errPayload := last.GetError()
	if errPayload == nil || errPayload.Message != "boom" {
		t.Fatalf("expected ERROR response with message 'boom', got %+v", last)
	}
}

func TestAgentCoreTransport_ElicitSurfacesAsError(t *testing.T) {
	srv, mock := newTestServer(t, "web")

	body := "data: {\"type\":\"START\"}\n\n" +
		"data: {\"type\":\"ELICIT\",\"renderableId\":\"r1\",\"message\":\"name?\"}\n\n"
	ts := httptest.NewServer(sseHandler(t, body))
	defer ts.Close()

	tx := NewAgentCoreTransport(srv, NewHTTPInvoker(ts.URL))
	if err := tx.HandleIncomingMessage(context.Background(), testMessage()); err != nil {
		t.Fatalf("HandleIncomingMessage: %v", err)
	}

	if last := mock.getLastResponse(); last.GetError() == nil {
		t.Fatalf("expected ELICIT to surface as an ERROR response, got %+v", last)
	}
}

func TestAgentCoreTransport_Non200IsError(t *testing.T) {
	srv, _ := newTestServer(t, "web")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	tx := NewAgentCoreTransport(srv, NewHTTPInvoker(ts.URL))
	if err := tx.HandleIncomingMessage(context.Background(), testMessage()); err == nil {
		t.Fatalf("expected error on non-200 invoke, got nil")
	}
}
