package a2a

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/astropods/messaging/internal/adapter"
	"github.com/astropods/messaging/internal/store"
	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
)

// newTestAdapter builds an adapter with a short send timeout so the blocking
// message/send path is testable, and no HTTP server: handlers are driven
// directly so tests never bind a port.
func newTestAdapter(t *testing.T, opts ...Option) *Adapter {
	t.Helper()
	base := []Option{
		WithAgentIdentity("billing-bot", "Answers billing questions.", "http://billing-bot.acct.svc.cluster.local:8100"),
		WithSendTimeout(2 * time.Second),
	}
	return New(append(base, opts...)...)
}

// chunk builds one content chunk of the agent's streamed reply.
func chunk(kind pb.ContentChunk_ChunkType, text string) *pb.ContentChunk {
	return &pb.ContentChunk{Type: kind, Content: text}
}

// replyingHandler returns a message handler that plays a scripted agent reply
// back through HandleAgentResponse, the same path the gRPC server uses.
func replyingHandler(a *Adapter, chunks ...*pb.ContentChunk) adapter.MessageHandler {
	return func(ctx context.Context, msg *pb.Message) error {
		for _, c := range chunks {
			_ = a.HandleAgentResponse(ctx, &pb.AgentResponse{
				ConversationId: msg.ConversationId,
				ResponseId:     "res-1",
				Payload:        &pb.AgentResponse_Content{Content: c},
			})
		}
		return nil
	}
}

// rpc issues one JSON-RPC call against the adapter's handler and decodes the
// envelope.
func rpc(t *testing.T, a *Adapter, method string, params any) response {
	t.Helper()
	body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		body["params"] = params
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(raw)))
	rec := httptest.NewRecorder()
	a.handleRPC(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("JSON-RPC transport must answer 200 even on error, got %d", rec.Code)
	}
	var res response
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode response: %v (body %s)", err, rec.Body.String())
	}
	return res
}

// resultTask decodes a successful result into a Task, failing the test on an
// unexpected JSON-RPC error.
func resultTask(t *testing.T, res response) Task {
	t.Helper()
	if res.Error != nil {
		t.Fatalf("expected a task result, got JSON-RPC error %d: %s", res.Error.Code, res.Error.Message)
	}
	raw, err := json.Marshal(res.Result)
	if err != nil {
		t.Fatalf("re-marshal result: %v", err)
	}
	var task Task
	if err := json.Unmarshal(raw, &task); err != nil {
		t.Fatalf("decode task: %v", err)
	}
	return task
}

func textMessage(text string) map[string]any {
	return map[string]any{
		"message": map[string]any{
			"role":      "user",
			"messageId": "msg-1",
			"parts":     []map[string]any{{"kind": "text", "text": text}},
		},
	}
}

func artifactText(task Task) string {
	var b strings.Builder
	for _, a := range task.Artifacts {
		for _, p := range a.Parts {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

func TestAgentCardIsServedWithTheAdvertisedIdentity(t *testing.T) {
	a := newTestAdapter(t)

	rec := httptest.NewRecorder()
	a.handleCard(rec, httptest.NewRequest(http.MethodGet, CardPath, nil))

	var card Card
	if err := json.Unmarshal(rec.Body.Bytes(), &card); err != nil {
		t.Fatalf("decode card: %v", err)
	}
	if card.ProtocolVersion != protocolVersion {
		t.Errorf("card protocolVersion = %q, want %q so A2A clients negotiate correctly", card.ProtocolVersion, protocolVersion)
	}
	if card.Name != "billing-bot" {
		t.Errorf("card name = %q, want the deployed agent name", card.Name)
	}
	if card.URL != "http://billing-bot.acct.svc.cluster.local:8100" {
		t.Errorf("card url = %q, want the absolute Service DNS URL peers dial; A2A requires it absolute", card.URL)
	}
	if card.Capabilities.Streaming {
		t.Error("card must advertise streaming=false while message/stream is unimplemented, or callers will use it and hang")
	}
}

func TestAgentCardDescriptionFallsBackToADerivedSentence(t *testing.T) {
	a := New(WithAgentIdentity("billing-bot", "", "http://x:8100"))

	rec := httptest.NewRecorder()
	a.handleCard(rec, httptest.NewRequest(http.MethodGet, CardPath, nil))

	var card Card
	if err := json.Unmarshal(rec.Body.Bytes(), &card); err != nil {
		t.Fatalf("decode card: %v", err)
	}
	if card.Description == "" {
		t.Error("description is required by A2A, so an unset A2A_AGENT_DESCRIPTION must still yield one")
	}
	if !strings.Contains(card.Description, "billing-bot") {
		t.Errorf("fallback description = %q, want it to name the agent", card.Description)
	}
}

func TestAgentCardSkillsComeFromTheToolsTheAgentDeclared(t *testing.T) {
	configStore := store.NewAgentConfigStore()
	configStore.Set(&pb.AgentConfig{Tools: []*pb.AgentToolConfig{
		{Name: "lookup_invoice", Title: "Look up an invoice", Description: "Fetch an invoice by number"},
		{Name: "refund", Description: "Issue a refund"},
		{Name: ""},
	}})
	a := newTestAdapter(t)
	a.SetAgentConfigStore(configStore)

	rec := httptest.NewRecorder()
	a.handleCard(rec, httptest.NewRequest(http.MethodGet, CardPath, nil))

	var card Card
	if err := json.Unmarshal(rec.Body.Bytes(), &card); err != nil {
		t.Fatalf("decode card: %v", err)
	}
	if len(card.Skills) != 2 {
		t.Fatalf("got %d skills, want 2: a tool with no name carries no skill identity and must be skipped", len(card.Skills))
	}
	if card.Skills[0].ID != "lookup_invoice" || card.Skills[0].Name != "Look up an invoice" {
		t.Errorf("first skill = %+v, want id from tool name and name from tool title", card.Skills[0])
	}
	if card.Skills[1].Name != "refund" {
		t.Errorf("second skill name = %q, want the tool name as fallback when the tool declares no title", card.Skills[1].Name)
	}
}

func TestSendReturnsACompletedTaskCarryingTheAgentReply(t *testing.T) {
	a := newTestAdapter(t)
	a.SetMessageHandler(replyingHandler(a,
		chunk(pb.ContentChunk_START, ""),
		chunk(pb.ContentChunk_DELTA, "Invoice 12 "),
		chunk(pb.ContentChunk_DELTA, "is paid."),
		chunk(pb.ContentChunk_END, ""),
	))

	task := resultTask(t, rpc(t, a, "message/send", textMessage("is invoice 12 paid?")))

	if task.Status.State != stateCompleted {
		t.Errorf("task state = %q, want %q once the agent sends END", task.Status.State, stateCompleted)
	}
	if got := artifactText(task); got != "Invoice 12 is paid." {
		t.Errorf("artifact text = %q, want every delta joined; agents stream a reply as deltas and send an empty END", got)
	}
	if task.Kind != "task" {
		t.Errorf("task kind = %q, want \"task\" so A2A clients discriminate the result union", task.Kind)
	}
}

func TestSendResetsTheReplyBufferOnReplaceSoOnlyTheFinalTextIsReturned(t *testing.T) {
	a := newTestAdapter(t)
	a.SetMessageHandler(replyingHandler(a,
		chunk(pb.ContentChunk_START, "draft"),
		chunk(pb.ContentChunk_REPLACE, "final answer"),
		chunk(pb.ContentChunk_END, ""),
	))

	task := resultTask(t, rpc(t, a, "message/send", textMessage("hello")))

	if got := artifactText(task); got != "final answer" {
		t.Errorf("artifact text = %q, want only the replacement; REPLACE overwrites everything streamed so far", got)
	}
}

func TestSendConcatenatesEveryTextPartOfTheCallerMessage(t *testing.T) {
	a := newTestAdapter(t)
	var seen string
	a.SetMessageHandler(func(ctx context.Context, msg *pb.Message) error {
		seen = msg.Content
		return a.HandleAgentResponse(ctx, &pb.AgentResponse{
			ConversationId: msg.ConversationId,
			Payload:        &pb.AgentResponse_Content{Content: chunk(pb.ContentChunk_END, "ok")},
		})
	})

	params := map[string]any{"message": map[string]any{
		"role":      "user",
		"messageId": "msg-1",
		"parts": []map[string]any{
			{"kind": "text", "text": "first "},
			{"kind": "data"},
			{"kind": "text", "text": "second"},
		},
	}}
	resultTask(t, rpc(t, a, "message/send", params))

	if seen != "first second" {
		t.Errorf("agent saw %q, want every text part joined; reading only parts[0] would truncate a multi-part message", seen)
	}
}

func TestSendReusesTheCallerContextIDAsTheConversationID(t *testing.T) {
	a := newTestAdapter(t)
	var seenConversation string
	a.SetMessageHandler(func(ctx context.Context, msg *pb.Message) error {
		seenConversation = msg.ConversationId
		return a.HandleAgentResponse(ctx, &pb.AgentResponse{
			ConversationId: msg.ConversationId,
			Payload:        &pb.AgentResponse_Content{Content: chunk(pb.ContentChunk_END, "ok")},
		})
	})

	params := map[string]any{"message": map[string]any{
		"role":      "user",
		"messageId": "msg-1",
		"contextId": "ctx-from-caller",
		"parts":     []map[string]any{{"kind": "text", "text": "hi"}},
	}}
	task := resultTask(t, rpc(t, a, "message/send", params))

	if seenConversation != "ctx-from-caller" {
		t.Errorf("agent saw conversation %q, want the caller's contextId so multi-turn A2A threading reaches the agent intact", seenConversation)
	}
	if task.ContextID != "ctx-from-caller" {
		t.Errorf("task contextId = %q, want the caller's value echoed back", task.ContextID)
	}
}

func TestSendMintsAContextIDWhenTheCallerOmitsOne(t *testing.T) {
	a := newTestAdapter(t)
	a.SetMessageHandler(replyingHandler(a, chunk(pb.ContentChunk_END, "ok")))

	task := resultTask(t, rpc(t, a, "message/send", textMessage("hi")))

	if task.ContextID == "" {
		t.Error("task contextId is empty; a first-turn caller sends no contextId and needs one minted to continue the thread")
	}
}

func TestSendRejectsAMessageWithNoText(t *testing.T) {
	a := newTestAdapter(t)
	a.SetMessageHandler(replyingHandler(a, chunk(pb.ContentChunk_END, "ok")))

	res := rpc(t, a, "message/send", map[string]any{"message": map[string]any{
		"role": "user", "messageId": "m", "parts": []map[string]any{{"kind": "data"}},
	}})

	if res.Error == nil || res.Error.Code != codeInvalidParams {
		t.Fatalf("got %+v, want InvalidParams (%d): the adapter handles text only and must say so rather than send an empty turn", res.Error, codeInvalidParams)
	}
}

func TestSendFailsTheTaskWhenNoAgentIsConnected(t *testing.T) {
	a := newTestAdapter(t)
	a.SetMessageHandler(func(ctx context.Context, msg *pb.Message) error {
		return adapter.ErrNoAgentStream
	})

	task := resultTask(t, rpc(t, a, "message/send", textMessage("hi")))

	if task.Status.State != stateFailed {
		t.Errorf("task state = %q, want %q when delivery never reached an agent", task.Status.State, stateFailed)
	}
	if !strings.Contains(task.Status.Message, "no agent is connected") {
		t.Errorf("task status message = %q, want a caller-readable reason rather than the raw sentinel error", task.Status.Message)
	}
}

func TestSendFailsTheTaskWhenTheAgentReportsAnError(t *testing.T) {
	a := newTestAdapter(t)
	a.SetMessageHandler(func(ctx context.Context, msg *pb.Message) error {
		return a.HandleAgentResponse(ctx, &pb.AgentResponse{
			ConversationId: msg.ConversationId,
			Payload: &pb.AgentResponse_Error{Error: &pb.ErrorResponse{
				Code: pb.ErrorResponse_TOOL_ERROR, Message: "invoice service timed out",
			}},
		})
	})

	task := resultTask(t, rpc(t, a, "message/send", textMessage("hi")))

	if task.Status.State != stateFailed {
		t.Errorf("task state = %q, want %q", task.Status.State, stateFailed)
	}
	if task.Status.Message != "invoice service timed out" {
		t.Errorf("task status message = %q, want the agent's own message surfaced to the caller", task.Status.Message)
	}
}

func TestSendRejectsACallBeforeTheAgentHasDialledIn(t *testing.T) {
	a := newTestAdapter(t)
	a.SetMessageHandler(replyingHandler(a, chunk(pb.ContentChunk_END, "ok")))
	a.SetAgentReadiness(func() bool { return false })

	res := rpc(t, a, "message/send", textMessage("hi"))

	if res.Error == nil {
		t.Fatal("want a JSON-RPC error while the agent is not connected, so the caller fails fast instead of waiting out sendTimeout")
	}
}

func TestSendTimesOutToAWorkingTaskThatLaterCompletesViaTasksGet(t *testing.T) {
	a := newTestAdapter(t, WithSendTimeout(30*time.Millisecond))
	var conversationID string
	a.SetMessageHandler(func(ctx context.Context, msg *pb.Message) error {
		conversationID = msg.ConversationId
		return nil
	})

	task := resultTask(t, rpc(t, a, "message/send", textMessage("slow one")))
	if task.Status.State != stateWorking && task.Status.State != stateSubmitted {
		t.Fatalf("task state = %q, want it still running: a slow turn must be pollable, not reported failed", task.Status.State)
	}

	if err := a.HandleAgentResponse(context.Background(), &pb.AgentResponse{
		ConversationId: conversationID,
		Payload:        &pb.AgentResponse_Content{Content: chunk(pb.ContentChunk_END, "done late")},
	}); err != nil {
		t.Fatalf("late agent response: %v", err)
	}

	polled := resultTask(t, rpc(t, a, "tasks/get", map[string]any{"id": task.ID}))
	if polled.Status.State != stateCompleted {
		t.Errorf("polled state = %q, want %q: the task must keep resolving after send returned", polled.Status.State, stateCompleted)
	}
	if got := artifactText(polled); got != "done late" {
		t.Errorf("polled artifact = %q, want the reply that arrived after the timeout", got)
	}
}

func TestTasksGetReportsTaskNotFoundForAnUnknownID(t *testing.T) {
	a := newTestAdapter(t)

	res := rpc(t, a, "tasks/get", map[string]any{"id": "nope"})

	if res.Error == nil || res.Error.Code != codeTaskNotFound {
		t.Fatalf("got %+v, want TaskNotFound (%d)", res.Error, codeTaskNotFound)
	}
}

func TestTasksCancelStopsAnInFlightTaskOnce(t *testing.T) {
	a := newTestAdapter(t, WithSendTimeout(20*time.Millisecond))
	a.SetMessageHandler(func(ctx context.Context, msg *pb.Message) error { return nil })

	task := resultTask(t, rpc(t, a, "message/send", textMessage("slow")))

	cancelled := resultTask(t, rpc(t, a, "tasks/cancel", map[string]any{"id": task.ID}))
	if cancelled.Status.State != stateCanceled {
		t.Errorf("state after cancel = %q, want %q", cancelled.Status.State, stateCanceled)
	}

	res := rpc(t, a, "tasks/cancel", map[string]any{"id": task.ID})
	if res.Error == nil || res.Error.Code != codeTaskNotCancelable {
		t.Fatalf("got %+v on second cancel, want TaskNotCancelable (%d)", res.Error, codeTaskNotCancelable)
	}
}

func TestStreamingMethodsReportUnsupportedOperation(t *testing.T) {
	a := newTestAdapter(t)
	a.SetMessageHandler(replyingHandler(a, chunk(pb.ContentChunk_END, "ok")))

	for _, method := range []string{"message/stream", "tasks/resubscribe"} {
		res := rpc(t, a, method, textMessage("hi"))
		if res.Error == nil || res.Error.Code != codeUnsupportedOperation {
			t.Errorf("%s returned %+v, want UnsupportedOperation (%d) to match capabilities.streaming=false", method, res.Error, codeUnsupportedOperation)
		}
	}
}

func TestUnknownMethodReportsMethodNotFound(t *testing.T) {
	a := newTestAdapter(t)

	res := rpc(t, a, "agent/doSomething", nil)

	if res.Error == nil || res.Error.Code != codeMethodNotFound {
		t.Fatalf("got %+v, want MethodNotFound (%d)", res.Error, codeMethodNotFound)
	}
}

func TestRequestWithWrongJSONRPCVersionIsRejected(t *testing.T) {
	a := newTestAdapter(t)

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"jsonrpc":"1.0","id":1,"method":"tasks/get"}`))
	rec := httptest.NewRecorder()
	a.handleRPC(rec, req)

	var res response
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if res.Error == nil || res.Error.Code != codeInvalidRequest {
		t.Fatalf("got %+v, want InvalidRequest (%d)", res.Error, codeInvalidRequest)
	}
}

func TestMalformedJSONIsReportedAsAParseError(t *testing.T) {
	a := newTestAdapter(t)

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{not json`))
	rec := httptest.NewRecorder()
	a.handleRPC(rec, req)

	var res response
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if res.Error == nil || res.Error.Code != codeParseError {
		t.Fatalf("got %+v, want ParseError (%d)", res.Error, codeParseError)
	}
}

func TestAgentDisconnectFailsTheInFlightTaskInsteadOfHanging(t *testing.T) {
	a := newTestAdapter(t, WithSendTimeout(5*time.Second))
	a.SetMessageHandler(func(ctx context.Context, msg *pb.Message) error {
		go func() {
			time.Sleep(10 * time.Millisecond)
			a.HandleAgentDisconnect(context.Background(), adapter.AgentStreamID)
		}()
		return nil
	})

	start := time.Now()
	task := resultTask(t, rpc(t, a, "message/send", textMessage("hi")))

	if task.Status.State != stateFailed {
		t.Errorf("task state = %q, want %q when the agent's stream ended mid-turn", task.Status.State, stateFailed)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("send took %s, want it to return on disconnect rather than wait out sendTimeout", elapsed)
	}
}
