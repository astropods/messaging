package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/astropods/messaging/internal/adapter"
	"github.com/astropods/messaging/internal/store"
	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
)

const interactionSchema = `{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`

// feedbackCapture records renderable responses (and any other feedback) the
// adapter forwards toward the agent.
type feedbackCapture struct {
	mu    sync.Mutex
	items []*pb.PlatformFeedback
}

func (f *feedbackCapture) handle(_ context.Context, fb *pb.PlatformFeedback) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items = append(f.items, fb)
	return nil
}

func (f *feedbackCapture) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.items)
}

func (f *feedbackCapture) last() *pb.PlatformFeedback {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.items) == 0 {
		return nil
	}
	return f.items[len(f.items)-1]
}

func drainSSE(conn *SSEConnection) []SSEEvent {
	var out []SSEEvent
	for {
		select {
		case e := <-conn.EventChan:
			out = append(out, e)
		case <-time.After(50 * time.Millisecond):
			return out
		}
	}
}

func hasEvent(events []SSEEvent, name string) bool {
	for _, e := range events {
		if e.Event == name {
			return true
		}
	}
	return false
}

func testRenderable(id string, actions ...pb.RenderableAction) *pb.Renderable {
	return &pb.Renderable{
		Id:             id,
		Kind:           pb.RenderKind_RENDER_KIND_FORM,
		Message:        "What is your name?",
		DataSchemaJson: interactionSchema,
		AllowedActions: actions,
	}
}

// testPermissionRenderable is a tool-approval ask (intent "tool_permission"), so
// its resolved note reads "Approved"/"Denied" rather than "Submitted"/"Declined".
func testPermissionRenderable(id string, actions ...pb.RenderableAction) *pb.Renderable {
	r := testRenderable(id, actions...)
	r.Intent = intentToolPermission
	return r
}

// attachConn registers an SSE connection on a connection manager so a test can
// drain the events broadcast to a conversation (e.g. the resolved-interaction note).
func attachConn(cm *ConnectionManager) *SSEConnection {
	conn := &SSEConnection{
		ID:             "conn-note",
		ConversationID: "conv",
		EventChan:      make(chan SSEEvent, 20),
		Done:           make(chan struct{}),
	}
	cm.Add(conn)
	return conn
}

// noteEventContent returns the content of the first note event, or ("", false)
// when none was broadcast.
func noteEventContent(t *testing.T, events []SSEEvent) (string, bool) {
	t.Helper()
	for _, e := range events {
		if e.Event != EventNote {
			continue
		}
		var d NoteEventData
		if err := json.Unmarshal([]byte(e.Data), &d); err != nil {
			t.Fatalf("unmarshal note event: %v", err)
		}
		return d.Content, true
	}
	return "", false
}

func newTestAdapter(t *testing.T, opts ...WebAdapterOption) (*WebAdapter, *SSEConnection, *feedbackCapture) {
	t.Helper()
	a := New(opts...)
	if err := a.Initialize(context.Background(), adapter.Config{}); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	fc := &feedbackCapture{}
	a.SetFeedbackHandler(fc.handle)

	conn := &SSEConnection{
		ID:             "conn-1",
		ConversationID: "conv",
		EventChan:      make(chan SSEEvent, 20),
		Done:           make(chan struct{}),
	}
	a.connManager.Add(conn)
	return a, conn, fc
}

// A Renderable persists and emits an SSE interaction event.
func TestRenderable_EmitsInteraction(t *testing.T) {
	its := store.NewMemoryInteractionStore()
	a, conn, fc := newTestAdapter(t, WithInteractionStore(its))

	resp := &pb.AgentResponse{
		ConversationId: "conv",
		Payload: &pb.AgentResponse_Renderable{
			Renderable: testRenderable("i1",
				pb.RenderableAction_RENDERABLE_ACTION_SUBMIT,
				pb.RenderableAction_RENDERABLE_ACTION_CANCEL),
		},
	}
	if err := a.HandleAgentResponse(context.Background(), resp); err != nil {
		t.Fatalf("HandleAgentResponse: %v", err)
	}

	if fc.count() != 0 {
		t.Errorf("capability-on emit must not send feedback, got %d", fc.count())
	}

	events := drainSSE(conn)
	if !hasEvent(events, EventInteraction) {
		t.Fatalf("expected an interaction SSE event, got %+v", events)
	}
	var data InteractionEventData
	for _, e := range events {
		if e.Event == EventInteraction {
			if err := json.Unmarshal([]byte(e.Data), &data); err != nil {
				t.Fatalf("unmarshal interaction event: %v", err)
			}
		}
	}
	if data.ID != "i1" || data.Kind != "form" {
		t.Errorf("interaction event: got id=%q kind=%q", data.ID, data.Kind)
	}
	if len(data.Actions) != 2 || data.Actions[0] != "submit" || data.Actions[1] != "cancel" {
		t.Errorf("actions: got %v, want [submit cancel]", data.Actions)
	}
	if len(data.DataSchema) == 0 {
		t.Errorf("data schema should be embedded as an object")
	}

	// Persisted as pending in the injected store.
	it, found, _ := its.GetInteraction(context.Background(), "conv", "i1")
	if !found || it.Status != store.InteractionPending {
		t.Errorf("interaction not persisted as pending: found=%v status=%q", found, it.Status)
	}
}

// A malformed data schema is dropped, not emitted or persisted.
func TestRenderable_MalformedSchemaDropped(t *testing.T) {
	a, conn, _ := newTestAdapter(t)

	r := testRenderable("i1", pb.RenderableAction_RENDERABLE_ACTION_SUBMIT, pb.RenderableAction_RENDERABLE_ACTION_CANCEL)
	r.DataSchemaJson = `{not valid json`
	resp := &pb.AgentResponse{
		ConversationId: "conv",
		Payload:        &pb.AgentResponse_Renderable{Renderable: r},
	}
	if err := a.HandleAgentResponse(context.Background(), resp); err != nil {
		t.Fatalf("HandleAgentResponse: %v", err)
	}

	if events := drainSSE(conn); hasEvent(events, EventInteraction) {
		t.Errorf("malformed renderable must not emit an interaction event")
	}
	if all, _ := a.interactions.ListInteractions(context.Background(), "conv"); len(all) != 0 {
		t.Errorf("malformed renderable must not persist, got %d", len(all))
	}
}

// A schema with an external $ref (e.g. file://) must not compile — it is dropped
// at emit, never persisted or shown, so it can't read the sidecar filesystem.
func TestRenderable_ExternalRefSchemaDropped(t *testing.T) {
	a, conn, _ := newTestAdapter(t)

	r := testRenderable("i1", pb.RenderableAction_RENDERABLE_ACTION_SUBMIT, pb.RenderableAction_RENDERABLE_ACTION_CANCEL)
	r.DataSchemaJson = `{"$ref":"file:///etc/passwd"}`
	resp := &pb.AgentResponse{
		ConversationId: "conv",
		Payload:        &pb.AgentResponse_Renderable{Renderable: r},
	}
	if err := a.HandleAgentResponse(context.Background(), resp); err != nil {
		t.Fatalf("HandleAgentResponse: %v", err)
	}

	if events := drainSSE(conn); hasEvent(events, EventInteraction) {
		t.Errorf("renderable with external $ref must not emit an interaction event")
	}
	if all, _ := a.interactions.ListInteractions(context.Background(), "conv"); len(all) != 0 {
		t.Errorf("renderable with external $ref must not persist, got %d", len(all))
	}
}

func TestCompileSchema_RejectsExternalRef(t *testing.T) {
	if _, err := compileSchema(`{"$ref":"file:///etc/passwd"}`); err == nil {
		t.Fatalf("compileSchema accepted a file:// $ref")
	}
}

// --- Response endpoint ---

func newEndpointHandlers(t *testing.T) (*Handlers, *store.MemoryInteractionStore, *feedbackCapture) {
	t.Helper()
	cm := NewConnectionManager(30 * time.Second)
	sm := NewHeaderSessionManager("X-User-ID", "", "")
	h := NewHandlers(cm, sm, nil, nil)
	its := store.NewMemoryInteractionStore()
	h.interactions = its
	fc := &feedbackCapture{}
	h.SetFeedbackHandler(fc.handle)
	return h, its, fc
}

func seedInteraction(t *testing.T, its *store.MemoryInteractionStore, userID string, actions ...pb.RenderableAction) {
	t.Helper()
	if _, err := its.AppendInteraction(context.Background(), "conv", userID, testRenderable("i1", actions...)); err != nil {
		t.Fatalf("seed interaction: %v", err)
	}
}

func interactionRequest(user, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/chat/conversations/conv/interactions/i1", strings.NewReader(body))
	if user != "" {
		req.Header.Set("X-User-ID", user)
	}
	req.SetPathValue("id", "conv")
	req.SetPathValue("interactionId", "i1")
	return req
}

func TestHandleInteractionResponse_SubmitValid(t *testing.T) {
	h, its, fc := newEndpointHandlers(t)
	seedInteraction(t, its, "alice",
		pb.RenderableAction_RENDERABLE_ACTION_SUBMIT,
		pb.RenderableAction_RENDERABLE_ACTION_CANCEL)

	w := httptest.NewRecorder()
	h.HandleInteractionResponse(w, interactionRequest("alice", `{"action":"submit","content":{"name":"octocat"}}`))

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%q", w.Code, w.Body.String())
	}
	if fc.count() != 1 {
		t.Fatalf("feedback count: got %d, want 1", fc.count())
	}
	rr := fc.last().GetRenderableResponse()
	if rr.GetAction() != pb.RenderableAction_RENDERABLE_ACTION_SUBMIT {
		t.Errorf("action: got %v, want SUBMIT", rr.GetAction())
	}
	if !strings.Contains(rr.GetContentJson(), "octocat") {
		t.Errorf("content_json missing payload: %q", rr.GetContentJson())
	}
	it, _, _ := its.GetInteraction(context.Background(), "conv", "i1")
	if it.Status != store.InteractionSubmitted {
		t.Errorf("status: got %q, want submitted", it.Status)
	}
}

func TestHandleInteractionResponse_SubmitInvalidSchema_422(t *testing.T) {
	h, its, fc := newEndpointHandlers(t)
	seedInteraction(t, its, "alice",
		pb.RenderableAction_RENDERABLE_ACTION_SUBMIT,
		pb.RenderableAction_RENDERABLE_ACTION_CANCEL)

	w := httptest.NewRecorder()
	// Missing required "name".
	h.HandleInteractionResponse(w, interactionRequest("alice", `{"action":"submit","content":{}}`))

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("want 422, got %d body=%q", w.Code, w.Body.String())
	}
	if fc.count() != 0 {
		t.Errorf("invalid content must not be delivered to the agent")
	}
	it, _, _ := its.GetInteraction(context.Background(), "conv", "i1")
	if it.Status != store.InteractionPending {
		t.Errorf("status changed on invalid submit: %q", it.Status)
	}
}

func TestHandleInteractionResponse_UnauthorizedResponder_403(t *testing.T) {
	h, its, fc := newEndpointHandlers(t)
	seedInteraction(t, its, "alice",
		pb.RenderableAction_RENDERABLE_ACTION_SUBMIT,
		pb.RenderableAction_RENDERABLE_ACTION_CANCEL)

	w := httptest.NewRecorder()
	h.HandleInteractionResponse(w, interactionRequest("bob", `{"action":"submit","content":{"name":"octocat"}}`))

	if w.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d body=%q", w.Code, w.Body.String())
	}
	if fc.count() != 0 {
		t.Errorf("wrong-user submission must not reach the agent")
	}
}

// An interaction whose owner never resolved (empty UserID) is answerable by no
// one — the endpoint fails closed rather than trusting any authenticated caller.
func TestHandleInteractionResponse_UnknownOwner_403(t *testing.T) {
	h, its, fc := newEndpointHandlers(t)
	seedInteraction(t, its, "", // unresolved owner
		pb.RenderableAction_RENDERABLE_ACTION_SUBMIT,
		pb.RenderableAction_RENDERABLE_ACTION_CANCEL)

	w := httptest.NewRecorder()
	h.HandleInteractionResponse(w, interactionRequest("alice", `{"action":"cancel"}`))

	if w.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d body=%q", w.Code, w.Body.String())
	}
	if fc.count() != 0 {
		t.Errorf("unknown-owner interaction must not reach the agent")
	}
}

func TestHandleInteractionResponse_Idempotent(t *testing.T) {
	h, its, fc := newEndpointHandlers(t)
	seedInteraction(t, its, "alice",
		pb.RenderableAction_RENDERABLE_ACTION_SUBMIT,
		pb.RenderableAction_RENDERABLE_ACTION_CANCEL)

	body := `{"action":"submit","content":{"name":"octocat"}}`
	w1 := httptest.NewRecorder()
	h.HandleInteractionResponse(w1, interactionRequest("alice", body))
	if w1.Code != http.StatusOK {
		t.Fatalf("first: want 200, got %d", w1.Code)
	}

	// Re-POST (e.g. after reload) returns the recorded result without re-delivering.
	w2 := httptest.NewRecorder()
	h.HandleInteractionResponse(w2, interactionRequest("alice", body))
	if w2.Code != http.StatusOK {
		t.Fatalf("second: want 200, got %d body=%q", w2.Code, w2.Body.String())
	}
	if fc.count() != 1 {
		t.Errorf("re-POST must not deliver a second response, got %d", fc.count())
	}
}

func TestHandleInteractionResponse_Decline(t *testing.T) {
	h, its, fc := newEndpointHandlers(t)
	seedInteraction(t, its, "alice",
		pb.RenderableAction_RENDERABLE_ACTION_SUBMIT,
		pb.RenderableAction_RENDERABLE_ACTION_DECLINE,
		pb.RenderableAction_RENDERABLE_ACTION_CANCEL)

	w := httptest.NewRecorder()
	h.HandleInteractionResponse(w, interactionRequest("alice", `{"action":"decline"}`))

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%q", w.Code, w.Body.String())
	}
	if rr := fc.last().GetRenderableResponse(); rr == nil || rr.GetAction() != pb.RenderableAction_RENDERABLE_ACTION_DECLINE {
		t.Fatalf("want DECLINE, got %+v", fc.last())
	}
	it, _, _ := its.GetInteraction(context.Background(), "conv", "i1")
	if it.Status != store.InteractionDeclined {
		t.Errorf("status: got %q, want declined", it.Status)
	}
}

// RESPOND cancels the current turn and queues the prose (delivered as a fresh turn); the agent must never receive a RESPOND response.
func TestHandleInteractionResponse_RespondCancelsAndQueues(t *testing.T) {
	h, its, fc := newEndpointHandlers(t)
	h.turns = newTurnTracker()
	h.turns.startTurn("conv")
	h.turns.enterAwaiting("conv")
	seedInteraction(t, its, "alice",
		pb.RenderableAction_RENDERABLE_ACTION_SUBMIT,
		pb.RenderableAction_RENDERABLE_ACTION_RESPOND,
		pb.RenderableAction_RENDERABLE_ACTION_CANCEL)

	w := httptest.NewRecorder()
	h.HandleInteractionResponse(w, interactionRequest("alice", `{"action":"respond","text":"Tuesday at 2pm"}`))

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%q", w.Code, w.Body.String())
	}
	// The agent is told to CANCEL its turn, never RESPOND.
	if rr := fc.last().GetRenderableResponse(); rr == nil || rr.GetAction() != pb.RenderableAction_RENDERABLE_ACTION_CANCEL {
		t.Fatalf("want CANCEL forwarded to agent, got %+v", fc.last())
	}
	// The prose is queued, surfaced when the cancelled turn reaches idle (endTurn).
	pending := h.turns.endTurn("conv")
	if pending == nil || pending.text != "Tuesday at 2pm" || pending.userID != "alice" {
		t.Fatalf("pending respond not queued: %+v", pending)
	}
	it, _, _ := its.GetInteraction(context.Background(), "conv", "i1")
	if it.Status == store.InteractionPending {
		t.Errorf("interaction should be resolved after respond, got %q", it.Status)
	}
}

func TestHandleInteractionResponse_ActionNotAllowed_400(t *testing.T) {
	h, its, fc := newEndpointHandlers(t)
	// RESPOND not offered.
	seedInteraction(t, its, "alice",
		pb.RenderableAction_RENDERABLE_ACTION_SUBMIT,
		pb.RenderableAction_RENDERABLE_ACTION_CANCEL)

	w := httptest.NewRecorder()
	h.HandleInteractionResponse(w, interactionRequest("alice", `{"action":"respond","text":"freeform"}`))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d body=%q", w.Code, w.Body.String())
	}
	if fc.count() != 0 {
		t.Errorf("disallowed action must not reach the agent")
	}
}

// CANCEL is always accepted as the dismiss escape, even if not explicitly listed.
func TestHandleInteractionResponse_CancelAlwaysAllowed(t *testing.T) {
	h, its, fc := newEndpointHandlers(t)
	seedInteraction(t, its, "alice", pb.RenderableAction_RENDERABLE_ACTION_SUBMIT)

	w := httptest.NewRecorder()
	h.HandleInteractionResponse(w, interactionRequest("alice", `{"action":"cancel"}`))

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%q", w.Code, w.Body.String())
	}
	if rr := fc.last().GetRenderableResponse(); rr == nil || rr.GetAction() != pb.RenderableAction_RENDERABLE_ACTION_CANCEL {
		t.Fatalf("want CANCEL, got %+v", fc.last())
	}
}

func TestHandleInteractionResponse_NotFound_404(t *testing.T) {
	h, _, _ := newEndpointHandlers(t)

	w := httptest.NewRecorder()
	h.HandleInteractionResponse(w, interactionRequest("alice", `{"action":"cancel"}`))

	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d body=%q", w.Code, w.Body.String())
	}
}

// --- Resolved-interaction ghost note ---

// SUBMIT and DECLINE broadcast a ghost note recording the answer before the turn resumes.
func TestHandleInteractionResponse_SubmitAndDeclineBroadcastNote(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
		actions          []pb.RenderableAction
	}{
		{"submit", `{"action":"submit","content":{"name":"octocat"}}`, "Answered · Name: octocat",
			[]pb.RenderableAction{pb.RenderableAction_RENDERABLE_ACTION_SUBMIT, pb.RenderableAction_RENDERABLE_ACTION_CANCEL}},
		{"decline", `{"action":"decline"}`, "Declined",
			[]pb.RenderableAction{pb.RenderableAction_RENDERABLE_ACTION_DECLINE, pb.RenderableAction_RENDERABLE_ACTION_CANCEL}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, its, _ := newEndpointHandlers(t)
			conn := attachConn(h.connManager)
			seedInteraction(t, its, "alice", tc.actions...)

			w := httptest.NewRecorder()
			h.HandleInteractionResponse(w, interactionRequest("alice", tc.body))
			if w.Code != http.StatusOK {
				t.Fatalf("want 200, got %d body=%q", w.Code, w.Body.String())
			}
			content, ok := noteEventContent(t, drainSSE(conn))
			if !ok || content != tc.want {
				t.Fatalf("note: got (%q, %v), want (%q, true)", content, ok, tc.want)
			}
		})
	}
}

// A tool-permission ask reads as approve/deny rather than submit/decline.
func TestHandleInteractionResponse_PermissionNoteWording(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
	}{
		{"approve", `{"action":"submit","content":{"name":"octocat"}}`, "Approved"},
		{"deny", `{"action":"decline"}`, "Denied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, its, _ := newEndpointHandlers(t)
			conn := attachConn(h.connManager)
			if _, err := its.AppendInteraction(context.Background(), "conv", "alice",
				testPermissionRenderable("i1",
					pb.RenderableAction_RENDERABLE_ACTION_SUBMIT,
					pb.RenderableAction_RENDERABLE_ACTION_DECLINE,
					pb.RenderableAction_RENDERABLE_ACTION_CANCEL)); err != nil {
				t.Fatalf("seed permission: %v", err)
			}

			w := httptest.NewRecorder()
			h.HandleInteractionResponse(w, interactionRequest("alice", tc.body))
			if w.Code != http.StatusOK {
				t.Fatalf("want 200, got %d body=%q", w.Code, w.Body.String())
			}
			content, ok := noteEventContent(t, drainSSE(conn))
			if !ok || content != tc.want {
				t.Fatalf("note: got (%q, %v), want (%q, true)", content, ok, tc.want)
			}
		})
	}
}

// CANCEL is a dismissal — no answer to record and the turn aborts — so it
// broadcasts no note.
func TestHandleInteractionResponse_CancelBroadcastsNoNote(t *testing.T) {
	h, its, _ := newEndpointHandlers(t)
	conn := attachConn(h.connManager)
	seedInteraction(t, its, "alice", pb.RenderableAction_RENDERABLE_ACTION_SUBMIT)

	w := httptest.NewRecorder()
	h.HandleInteractionResponse(w, interactionRequest("alice", `{"action":"cancel"}`))
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%q", w.Code, w.Body.String())
	}
	if content, ok := noteEventContent(t, drainSSE(conn)); ok {
		t.Fatalf("cancel must not broadcast a note, got %q", content)
	}
}

// RESPOND defers its note to injectRespond, so the endpoint itself broadcasts none.
func TestHandleInteractionResponse_RespondDefersNote(t *testing.T) {
	h, its, _ := newEndpointHandlers(t)
	h.turns = newTurnTracker()
	h.turns.startTurn("conv")
	h.turns.enterAwaiting("conv")
	conn := attachConn(h.connManager)
	seedInteraction(t, its, "alice",
		pb.RenderableAction_RENDERABLE_ACTION_SUBMIT,
		pb.RenderableAction_RENDERABLE_ACTION_RESPOND,
		pb.RenderableAction_RENDERABLE_ACTION_CANCEL)

	w := httptest.NewRecorder()
	h.HandleInteractionResponse(w, interactionRequest("alice", `{"action":"respond","text":"Tuesday at 2pm"}`))
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%q", w.Code, w.Body.String())
	}
	if content, ok := noteEventContent(t, drainSSE(conn)); ok {
		t.Fatalf("respond must not broadcast a note at endpoint time, got %q", content)
	}
}

func TestSummarizeSubmission(t *testing.T) {
	schema := `{"type":"object","properties":{
		"meetingDate":{"type":"string","title":"Date"},
		"attendee_count":{"type":"integer"},
		"recurring":{"type":"boolean"}
	}}`
	// Schema title wins ("Date"); no-title keys humanize; scalars format (int, yes/no); keys sorted for determinism.
	got := summarizeSubmission(`{"meetingDate":"2027-06-23","attendee_count":12,"recurring":true}`, schema)
	if got != "Attendee count: 12 · Date: 2027-06-23 · Recurring: yes" {
		t.Fatalf("summary: got %q", got)
	}
}

func TestSummarizeSubmission_NonObjectOrEmpty(t *testing.T) {
	if s := summarizeSubmission(`[]`, `{}`); s != "" {
		t.Errorf("array content: got %q, want empty", s)
	}
	if s := summarizeSubmission(`{}`, `{}`); s != "" {
		t.Errorf("empty object: got %q, want empty", s)
	}
	// Non-scalar fields are omitted, leaving nothing to summarize.
	if s := summarizeSubmission(`{"tags":["a","b"]}`, `{}`); s != "" {
		t.Errorf("non-scalar field: got %q, want empty", s)
	}
}

// injectRespond records the prose as the ghost note and forwards it to the agent
// as the follow-up turn.
func TestInjectRespond_NotesProseAndForwards(t *testing.T) {
	h, _, _ := newEndpointHandlers(t)
	h.turns = newTurnTracker()
	conn := attachConn(h.connManager)
	var forwarded string
	h.SetMessageHandler(func(_ context.Context, m *pb.Message) error {
		forwarded = m.Content
		return nil
	})

	h.injectRespond(context.Background(), "conv", &pendingRespond{userID: "alice", text: "Tuesday at 2pm"})

	content, ok := noteEventContent(t, drainSSE(conn))
	if !ok || content != "Tuesday at 2pm" {
		t.Fatalf("respond note: got (%q, %v), want the prose", content, ok)
	}
	if forwarded != "Tuesday at 2pm" {
		t.Fatalf("prose not forwarded to agent: %q", forwarded)
	}
}

// A failed follow-up forward must resolve the live stream with an error and clear the turn, not leave the client hung.
func TestInjectRespond_ForwardFailureSurfacesError(t *testing.T) {
	h, _, _ := newEndpointHandlers(t)
	h.turns = newTurnTracker()
	h.turns.startTurn("conv")
	conn := attachConn(h.connManager)
	h.SetMessageHandler(func(_ context.Context, _ *pb.Message) error {
		return adapter.ErrNoAgentStream
	})

	h.injectRespond(context.Background(), "conv", &pendingRespond{userID: "alice", text: "Tuesday at 2pm"})

	events := drainSSE(conn)
	if content, ok := noteEventContent(t, events); !ok || content != "Tuesday at 2pm" {
		t.Fatalf("respond note still recorded: got (%q, %v)", content, ok)
	}
	if !hasEvent(events, EventError) {
		t.Fatalf("forward failure must broadcast an error event; got %+v", events)
	}
	if h.turns.isStreaming("conv") {
		t.Errorf("turn should be cleared after a failed forward")
	}
}
