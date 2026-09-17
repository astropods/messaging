package peers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/astropods/messaging/internal/a2awire"
)

// peerServer stands in for another agent's A2A endpoint. reply is invoked with
// the decoded message/send params and returns whatever the peer should answer.
func peerServer(t *testing.T, reply func(params a2awire.SendParams) a2awire.Response) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /", func(w http.ResponseWriter, r *http.Request) {
		var req a2awire.Request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("peer got undecodable request: %v", err)
			return
		}
		if req.Method != "message/send" {
			t.Errorf("peer got method %q, want message/send", req.Method)
		}
		var params a2awire.SendParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			t.Errorf("peer got undecodable params: %v", err)
			return
		}
		res := reply(params)
		res.ID = req.ID
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(res); err != nil {
			t.Errorf("peer encode: %v", err)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func completedTask(contextID, text string) a2awire.Response {
	return a2awire.ResultResponse(nil, a2awire.Task{
		ID:        "task-1",
		ContextID: contextID,
		Kind:      "task",
		Status:    a2awire.TaskStatus{State: a2awire.StateCompleted},
		Artifacts: []a2awire.Artifact{{
			ArtifactID: "art-1",
			Parts:      []a2awire.Part{{Kind: a2awire.PartKindText, Text: text}},
		}},
	})
}

func TestAskSendsTheMessageAsATextPartAndReturnsTheReply(t *testing.T) {
	var seen string
	srv := peerServer(t, func(params a2awire.SendParams) a2awire.Response {
		seen = params.Message.Text()
		return completedTask("ctx-1", "Invoice 12 is paid.")
	})

	reply, err := NewClient().Ask(context.Background(), srv.URL, "", "is invoice 12 paid?")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}

	if seen != "is invoice 12 paid?" {
		t.Errorf("peer received %q, want the caller's message as a text part", seen)
	}
	if reply.Text != "Invoice 12 is paid." {
		t.Errorf("reply text = %q, want the peer's artifact text", reply.Text)
	}
	if !reply.Done {
		t.Error("Done = false for a completed task; the caller would needlessly poll")
	}
	if reply.ContextID != "ctx-1" {
		t.Errorf("ContextID = %q, want the peer's context so a follow-up threads", reply.ContextID)
	}
}

func TestAskThreadsOntoAnExistingConversation(t *testing.T) {
	var seen string
	srv := peerServer(t, func(params a2awire.SendParams) a2awire.Response {
		seen = params.Message.ContextID
		return completedTask(params.Message.ContextID, "ok")
	})

	if _, err := NewClient().Ask(context.Background(), srv.URL, "ctx-existing", "follow up"); err != nil {
		t.Fatalf("Ask: %v", err)
	}

	if seen != "ctx-existing" {
		t.Errorf("peer received contextId %q, want the caller's; otherwise every turn starts a new thread", seen)
	}
}

func TestAskJoinsTextAcrossArtifactsAndParts(t *testing.T) {
	srv := peerServer(t, func(a2awire.SendParams) a2awire.Response {
		return a2awire.ResultResponse(nil, a2awire.Task{
			ID: "task-1", Kind: "task",
			Status: a2awire.TaskStatus{State: a2awire.StateCompleted},
			Artifacts: []a2awire.Artifact{
				{Parts: []a2awire.Part{
					{Kind: a2awire.PartKindText, Text: "part one "},
					{Kind: "data"},
				}},
				{Parts: []a2awire.Part{{Kind: a2awire.PartKindText, Text: "part two"}}},
			},
		})
	})

	reply, err := NewClient().Ask(context.Background(), srv.URL, "", "hi")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if reply.Text != "part one part two" {
		t.Errorf("reply text = %q, want every text part joined; A2A may split a reply across artifacts", reply.Text)
	}
}

func TestAskReportsAStillWorkingTaskWithoutFailing(t *testing.T) {
	srv := peerServer(t, func(a2awire.SendParams) a2awire.Response {
		return a2awire.ResultResponse(nil, a2awire.Task{
			ID: "task-slow", ContextID: "ctx-1", Kind: "task",
			Status: a2awire.TaskStatus{State: a2awire.StateWorking},
		})
	})

	reply, err := NewClient().Ask(context.Background(), srv.URL, "", "slow one")

	if err != nil {
		t.Fatalf("Ask returned %v; a slow peer is not a failure, the caller can poll", err)
	}
	if reply.Done {
		t.Error("Done = true for a working task")
	}
	if reply.TaskID != "task-slow" {
		t.Errorf("TaskID = %q, want the peer's task id so the caller can poll it", reply.TaskID)
	}
}

func TestAskSurfacesTheReasonAPeerFailedTheTask(t *testing.T) {
	srv := peerServer(t, func(a2awire.SendParams) a2awire.Response {
		return a2awire.ResultResponse(nil, a2awire.Task{
			ID: "task-1", Kind: "task",
			Status: a2awire.TaskStatus{State: a2awire.StateFailed, Message: "invoice service timed out"},
		})
	})

	_, err := NewClient().Ask(context.Background(), srv.URL, "", "hi")

	if err == nil {
		t.Fatal("want an error for a failed task")
	}
	if !strings.Contains(err.Error(), "invoice service timed out") {
		t.Errorf("error = %v, want the peer's own reason so the caller can report it", err)
	}
}

func TestAskSurfacesAJSONRPCRejection(t *testing.T) {
	srv := peerServer(t, func(a2awire.SendParams) a2awire.Response {
		return a2awire.ErrorResponse(nil, a2awire.CodeInternalError, "agent is not connected")
	})

	_, err := NewClient().Ask(context.Background(), srv.URL, "", "hi")

	if err == nil {
		t.Fatal("want an error when the peer rejects the call")
	}
	if !strings.Contains(err.Error(), "agent is not connected") {
		t.Errorf("error = %v, want the peer's rejection message", err)
	}
}

func TestAskRejectsEmptyInputBeforeCallingAnyone(t *testing.T) {
	called := false
	srv := peerServer(t, func(a2awire.SendParams) a2awire.Response {
		called = true
		return completedTask("", "")
	})

	if _, err := NewClient().Ask(context.Background(), srv.URL, "", "   "); err == nil {
		t.Error("want an error for whitespace-only text")
	}
	if _, err := NewClient().Ask(context.Background(), "", "", "hi"); err == nil {
		t.Error("want an error for an empty peer URL")
	}
	if called {
		t.Error("a peer was called despite invalid input")
	}
}

func TestCardFetchesThePeersDescriptionAndSkills(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/agent-card.json", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(a2awire.Card{
			Name:        "billing-bot",
			Description: "Answers billing questions.",
			Skills:      []a2awire.Skill{{ID: "lookup", Name: "Look up an invoice"}},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	card, err := NewClient().Card(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Card: %v", err)
	}
	if card.Description != "Answers billing questions." {
		t.Errorf("description = %q, want the peer's own", card.Description)
	}
	if len(card.Skills) != 1 || card.Skills[0].Name != "Look up an invoice" {
		t.Errorf("skills = %+v, want the peer's declared skill", card.Skills)
	}
}

func TestCardErrorsWhenThePeerHasNoCard(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)

	if _, err := NewClient().Card(context.Background(), srv.URL); err == nil {
		t.Fatal("want an error when the card is missing, so the caller falls back to the name alone")
	}
}
