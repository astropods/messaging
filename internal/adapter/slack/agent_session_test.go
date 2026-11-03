package slack

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
	"github.com/slack-go/slack/slackevents"
)

func newStopTestAdapter(t *testing.T) (*SlackAdapter, *[]slackCall, *[]*pb.PlatformFeedback) {
	t.Helper()
	a, calls, cleanup := newTestSlackAdapter(t)
	t.Cleanup(cleanup)
	var feedback []*pb.PlatformFeedback
	a.feedbackHandler = func(_ context.Context, fb *pb.PlatformFeedback) error {
		feedback = append(feedback, fb)
		return nil
	}
	return a, calls, &feedback
}

func callsTo(calls *[]slackCall, method string) []string {
	var bodies []string
	for _, c := range *calls {
		if c.Method == "/"+method {
			raw, _ := json.Marshal(c.Body)
			bodies = append(bodies, string(raw))
		}
	}
	return bodies
}

func chunk(conversationID string, kind pb.ContentChunk_ChunkType, text string) *pb.AgentResponse {
	return &pb.AgentResponse{
		ConversationId: conversationID,
		Payload:        &pb.AgentResponse_Content{Content: &pb.ContentChunk{Type: kind, Content: text}},
	}
}

func respond(t *testing.T, a *SlackAdapter, responses ...*pb.AgentResponse) {
	t.Helper()
	for _, r := range responses {
		if err := a.HandleAgentResponse(t.Context(), r); err != nil {
			t.Fatalf("HandleAgentResponse: %v", err)
		}
	}
}

func TestAgentSessionStopped_EventReachesAgentAsStop(t *testing.T) {
	a, calls, feedback := newStopTestAdapter(t)
	payload := `{"type":"event_callback","team_id":"T1","event":{"type":"agent_session_stopped",` +
		`"channel":"C1","thread_ts":"1700000000.000001","user":"U1","event_ts":"1700000000.000005","streaming_message_ts":[]}}`
	parsed, err := slackevents.ParseEvent(json.RawMessage(payload), slackevents.OptionNoVerifyToken())
	if err != nil {
		t.Fatalf("ParseEvent: %v", err)
	}

	a.handleInnerEvent(t.Context(), parsed.InnerEvent, parsed.TeamID, json.RawMessage(payload))

	if len(*feedback) != 1 {
		t.Fatalf("a Stop press must forward one feedback event to the agent, got %d", len(*feedback))
	}
	fb := (*feedback)[0]
	if fb.GetConversationId() != "C1-1700000000.000001" {
		t.Errorf("STOP must name the session's thread conversation, got %q", fb.GetConversationId())
	}
	if fb.GetStreamControl().GetAction() != pb.StreamControl_STOP {
		t.Errorf("feedback must be a StreamControl STOP, got %v", fb.GetFeedback())
	}
	if fb.GetUser().GetId() != "U1" {
		t.Errorf("STOP must carry the Slack user who pressed Stop, got %q", fb.GetUser().GetId())
	}

	setStatus := callsTo(calls, "agents.sessions.setStatus")
	if len(setStatus) != 1 {
		t.Fatalf("the session must leave processing, or Slack keeps showing Stop; got %d setStatus calls", len(setStatus))
	}
	for _, want := range []string{`"status":"active"`, `"channel_id":"C1"`, `"thread_ts":"1700000000.000001"`} {
		if !strings.Contains(setStatus[0], want) {
			t.Errorf("setStatus must send %s, got %s", want, setStatus[0])
		}
	}
}

func TestAgentSessionStopped_DropsTheStoppedTurnUntilTheNextStart(t *testing.T) {
	a, calls, _ := newStopTestAdapter(t)
	const conv = "D1-1700000000.000001"

	respond(t, a, chunk(conv, pb.ContentChunk_START, ""), chunk(conv, pb.ContentChunk_DELTA, "partial "))
	a.handleAgentSessionStopped(t.Context(), &slackevents.AgentSessionStoppedEvent{
		Channel: "D1", ThreadTimestamp: "1700000000.000001", User: "U1",
	})
	if _, held := a.contentBuffers[conv]; held {
		t.Error("the stopped turn's partial reply must be released, since an aborted agent sends no END")
	}
	respond(t, a,
		&pb.AgentResponse{ConversationId: conv, Payload: &pb.AgentResponse_Status{Status: &pb.StatusUpdate{Status: pb.StatusUpdate_THINKING}}},
		chunk(conv, pb.ContentChunk_DELTA, "late"),
		chunk(conv, pb.ContentChunk_START, ""),
		chunk(conv, pb.ContentChunk_DELTA, "next answer"),
		chunk(conv, pb.ContentChunk_END, ""),
	)

	posts := callsTo(calls, "chat.postMessage")
	if len(posts) != 1 {
		t.Fatalf("only the turn after the stop may post, got %d posts", len(posts))
	}
	if !strings.Contains(posts[0], "next answer") {
		t.Errorf("the next turn's reply must post, got %s", posts[0])
	}
	for _, stale := range []string{"partial", "late"} {
		if strings.Contains(posts[0], stale) {
			t.Errorf("the stopped turn's text %q leaked into the next reply: %s", stale, posts[0])
		}
	}
	if n := len(callsTo(calls, "assistant.threads.setStatus")); n != 0 {
		t.Errorf("a stopped turn's status update must not reopen the loading state, got %d calls", n)
	}
}

func TestAgentSessionStopped_EndOrErrorClosesTheStoppedTurn(t *testing.T) {
	for _, closing := range []*pb.AgentResponse{
		chunk("D1-1.1", pb.ContentChunk_END, "ignored the stop"),
		{ConversationId: "D1-1.1", Payload: &pb.AgentResponse_Error{Error: &pb.ErrorResponse{Message: "ignored the stop"}}},
	} {
		a, calls, _ := newStopTestAdapter(t)
		a.handleAgentSessionStopped(t.Context(), &slackevents.AgentSessionStoppedEvent{Channel: "D1", ThreadTimestamp: "1.1"})

		respond(t, a, closing, chunk("D1-1.1", pb.ContentChunk_DELTA, "after"), chunk("D1-1.1", pb.ContentChunk_END, ""))

		posts := callsTo(calls, "chat.postMessage")
		if len(posts) != 1 || strings.Contains(posts[0], "ignored the stop") {
			t.Fatalf("%T must close the stopped turn without posting, so later output posts once; got %d posts", closing.Payload, len(posts))
		}
		if !strings.Contains(posts[0], "after") {
			t.Errorf("output after the stopped turn closed must post, got %s", posts[0])
		}
	}
}

func TestStoppedConversation(t *testing.T) {
	tests := []struct {
		name     string
		inFlight []string
		channel  string
		threadTS string
		want     string
	}{
		{"channel thread", nil, "C1", "1.1", "C1-1.1"},
		{"DM thread with its turn in flight", []string{"D1-1.1", "D1"}, "D1", "1.1", "D1-1.1"},
		{"top-level DM message with the DM turn in flight", []string{"D1"}, "D1", "1.1", "D1"},
		{"DM with no turn in flight", nil, "D1", "1.1", "D1-1.1"},
		{"channel thread ignores a DM-style key", []string{"C1"}, "C1", "1.1", "C1-1.1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, _ := newTestAdapter()
			for _, id := range tt.inFlight {
				a.contentBuffers[id] = ""
			}
			if got := a.stoppedConversation(tt.channel, tt.threadTS); got != tt.want {
				t.Errorf("stoppedConversation(%q, %q) = %q, want %q", tt.channel, tt.threadTS, got, tt.want)
			}
		})
	}
}

func TestStopTurn_BoundsTrackedStops(t *testing.T) {
	a, _ := newTestAdapter()
	for i := range maxStoppedTurns + 10 {
		a.stopTurn(strings.Repeat("x", i+1))
	}
	if n := len(a.stoppedTurns); n != maxStoppedTurns {
		t.Errorf("stopped turns that never restart must stay bounded at %d, got %d", maxStoppedTurns, n)
	}
}
