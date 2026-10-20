package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/astropods/messaging/internal/store"
	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
)

func agentWithModels(models ...string) *store.AgentConfigStore {
	cs := store.NewAgentConfigStore()
	cs.Set(&pb.AgentConfig{Models: models})
	return cs
}

func modelSendRequest(model string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/conversations/conv-1/messages",
		strings.NewReader(`{"content":"hi","model":`+strconvQuote(model)+`}`))
	req.Header.Set("X-User-ID", "user-1")
	req.SetPathValue("id", "conv-1")
	return req
}

func modelHandlers(t *testing.T, cs *store.AgentConfigStore) (*Handlers, *[]*pb.Message) {
	t.Helper()
	h, _ := newChatTitleHandlers(t)
	h.agentConfigStore = cs
	var forwarded []*pb.Message
	h.SetMessageHandler(func(_ context.Context, msg *pb.Message) error {
		forwarded = append(forwarded, msg)
		return nil
	})
	return h, &forwarded
}

func TestASendCarriesTheModelPickedToTheAgent(t *testing.T) {
	h, forwarded := modelHandlers(t, agentWithModels("claude-sonnet-4-6", "claude-opus-4-7"))

	w := httptest.NewRecorder()
	h.HandleSendMessage(w, modelSendRequest("claude-opus-4-7"))

	if w.Code != http.StatusOK {
		t.Fatalf("send: got %d %s", w.Code, w.Body.String())
	}
	if got := (*forwarded)[0].GetModel(); got != "claude-opus-4-7" {
		t.Fatalf("forwarded model = %q, want claude-opus-4-7", got)
	}
}

func TestASendWithoutAModelLeavesTheChoiceToTheAgent(t *testing.T) {
	h, forwarded := modelHandlers(t, agentWithModels("claude-sonnet-4-6", "claude-opus-4-7"))

	w := httptest.NewRecorder()
	h.HandleSendMessage(w, sendMessageRequest("user-1", "conv-1", "hi"))

	if w.Code != http.StatusOK {
		t.Fatalf("send: got %d %s", w.Code, w.Body.String())
	}
	if got := (*forwarded)[0].GetModel(); got != "" {
		t.Fatalf("forwarded model = %q, want none", got)
	}
}

func TestASendWithAModelTheAgentDoesNotOfferIsRejectedBeforeTheAgent(t *testing.T) {
	cases := map[string]*store.AgentConfigStore{
		"not in the agent's list":  agentWithModels("claude-sonnet-4-6", "claude-opus-4-7"),
		"agent declares no models": agentWithModels(),
		"no agent config yet":      store.NewAgentConfigStore(),
	}
	for name, cs := range cases {
		t.Run(name, func(t *testing.T) {
			h, forwarded := modelHandlers(t, cs)

			w := httptest.NewRecorder()
			h.HandleSendMessage(w, modelSendRequest("gpt-4o"))

			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "invalid_model") {
				t.Fatalf("got %d %s, want 400 invalid_model", w.Code, w.Body.String())
			}
			if len(*forwarded) != 0 {
				t.Fatal("a model the agent does not offer reached the agent")
			}
		})
	}
}

func TestHandleAgentConfigReportsTheAgentsModels(t *testing.T) {
	models := func(cs *store.AgentConfigStore) []string {
		h := NewHandlers(NewConnectionManager(time.Second), &NoopSessionManager{}, nil, cs)
		w := httptest.NewRecorder()
		h.HandleAgentConfig(w, httptest.NewRequest(http.MethodGet, "/api/agent/config", nil))
		var resp struct {
			Capabilities struct {
				Models []string `json:"models"`
			} `json:"capabilities"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode agent/config: %v", err)
		}
		return resp.Capabilities.Models
	}

	want := []string{"claude-sonnet-4-6", "claude-opus-4-7"}
	if got := models(agentWithModels(want...)); !slices.Equal(got, want) {
		t.Errorf("capabilities.models = %v, want %v", got, want)
	}
	if got := models(agentWithModels()); got != nil {
		t.Errorf("capabilities.models for an agent with none = %v, want absent", got)
	}
}
