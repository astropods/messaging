package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/astropods/messaging/internal/adapter"
	"github.com/astropods/messaging/internal/store"
	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
)

const chatModelsJSON = `{"default":{"entry":"default","model":"sonnet"},"options":[{"entry":"default","model":"sonnet"},{"entry":"default","model":"opus"}]}`

func testChatModels(t *testing.T) *ChatModels {
	t.Helper()
	cm, err := ParseChatModels(chatModelsJSON)
	if err != nil {
		t.Fatalf("parse chat models: %v", err)
	}
	return cm
}

func TestParseChatModels(t *testing.T) {
	cm, err := ParseChatModels(chatModelsJSON)
	if err != nil {
		t.Fatalf("valid CHAT_MODELS: %v", err)
	}
	want := &ChatModels{
		Default: ModelChoice{Entry: "default", Model: "sonnet"},
		Options: []ModelChoice{{Entry: "default", Model: "sonnet"}, {Entry: "default", Model: "opus"}},
	}
	if !reflect.DeepEqual(cm, want) {
		t.Fatalf("ParseChatModels = %+v, want %+v", cm, want)
	}

	for name, raw := range map[string]string{
		"not json":           `[`,
		"no options":         `{"default":{"entry":"default","model":"sonnet"},"options":[]}`,
		"option no model":    `{"default":{"entry":"default","model":"sonnet"},"options":[{"entry":"default","model":"sonnet"},{"entry":"default"}]}`,
		"default not listed": `{"default":{"entry":"default","model":"haiku"},"options":[{"entry":"default","model":"sonnet"}]}`,
	} {
		if _, err := ParseChatModels(raw); err == nil {
			t.Errorf("%s: ParseChatModels(%s) succeeded, want an error", name, raw)
		}
	}
}

func modelSendRequest(model string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/conversations/conv-1/messages",
		strings.NewReader(`{"content":"hi","model":`+model+`}`))
	req.Header.Set("X-User-ID", "user-1")
	req.SetPathValue("id", "conv-1")
	return req
}

func modelHandlers(t *testing.T, cm *ChatModels) (*Handlers, *[]*pb.Message) {
	t.Helper()
	h, _ := newChatTitleHandlers(t)
	h.chatModels = cm
	var forwarded []*pb.Message
	h.SetMessageHandler(func(_ context.Context, msg *pb.Message) error {
		forwarded = append(forwarded, msg)
		return nil
	})
	return h, &forwarded
}

func TestASendCarriesTheChosenModelToTheAgent(t *testing.T) {
	h, forwarded := modelHandlers(t, testChatModels(t))

	w := httptest.NewRecorder()
	h.HandleSendMessage(w, modelSendRequest(`{"entry":"default","model":"opus"}`))

	if w.Code != http.StatusOK {
		t.Fatalf("send: got %d %s", w.Code, w.Body.String())
	}
	if got := (*forwarded)[0].GetModel(); got.GetEntry() != "default" || got.GetModel() != "opus" {
		t.Fatalf("forwarded model = %v, want default/opus", got)
	}
}

func TestASendWithoutAModelCarriesTheDefault(t *testing.T) {
	h, forwarded := modelHandlers(t, testChatModels(t))

	w := httptest.NewRecorder()
	h.HandleSendMessage(w, sendMessageRequest("user-1", "conv-1", "hi"))

	if w.Code != http.StatusOK {
		t.Fatalf("send: got %d %s", w.Code, w.Body.String())
	}
	if got := (*forwarded)[0].GetModel(); got.GetModel() != "sonnet" {
		t.Fatalf("forwarded model = %v, want the default sonnet", got)
	}
}

func TestASendWithAnUnofferedModelIsRejectedBeforeTheAgent(t *testing.T) {
	cases := map[string]*ChatModels{
		"not listed":           testChatModels(t),
		"no models configured": nil,
	}
	for name, cm := range cases {
		t.Run(name, func(t *testing.T) {
			h, forwarded := modelHandlers(t, cm)

			w := httptest.NewRecorder()
			h.HandleSendMessage(w, modelSendRequest(`{"entry":"default","model":"haiku"}`))

			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "invalid_model") {
				t.Fatalf("got %d %s, want 400 invalid_model", w.Code, w.Body.String())
			}
			if len(*forwarded) != 0 {
				t.Fatalf("a rejected model reached the agent")
			}
		})
	}
}

func TestASendWithoutChatModelsCarriesNoModel(t *testing.T) {
	h, forwarded := modelHandlers(t, nil)

	w := httptest.NewRecorder()
	h.HandleSendMessage(w, sendMessageRequest("user-1", "conv-1", "hi"))

	if w.Code != http.StatusOK {
		t.Fatalf("send: got %d %s", w.Code, w.Body.String())
	}
	if got := (*forwarded)[0].GetModel(); got != nil {
		t.Fatalf("forwarded model = %v, want none", got)
	}
}

func TestAWriteYourOwnReplyTurnCarriesTheDefaultModel(t *testing.T) {
	h, _, _ := newEndpointHandlers(t)
	h.turns = newTurnTracker()
	h.chatModels = testChatModels(t)
	var forwarded *pb.Message
	h.SetMessageHandler(func(_ context.Context, m *pb.Message) error {
		forwarded = m
		return nil
	})

	h.injectRespond(context.Background(), "conv", &pendingRespond{userID: "alice", text: "Tuesday"})

	if forwarded.GetModel().GetModel() != "sonnet" {
		t.Fatalf("respond turn model = %v, want the default sonnet", forwarded.GetModel())
	}
}

func TestHandleAgentConfigReportsChatModels(t *testing.T) {
	models := func(cm *ChatModels) *ChatModels {
		cs := store.NewAgentConfigStore()
		cs.Set(&pb.AgentConfig{})
		h := NewHandlers(NewConnectionManager(time.Second), &NoopSessionManager{}, nil, cs)
		h.chatModels = cm
		w := httptest.NewRecorder()
		h.HandleAgentConfig(w, httptest.NewRequest(http.MethodGet, "/api/agent/config", nil))
		var resp struct {
			Capabilities struct {
				Models *ChatModels `json:"models"`
			} `json:"capabilities"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode agent/config: %v", err)
		}
		return resp.Capabilities.Models
	}

	cm := testChatModels(t)
	if got := models(cm); !reflect.DeepEqual(got, cm) {
		t.Errorf("capabilities.models = %+v, want %+v", got, cm)
	}
	if got := models(nil); got != nil {
		t.Errorf("capabilities.models without CHAT_MODELS = %+v, want absent", got)
	}
}

func TestWithChatModelsReachesTheHandlers(t *testing.T) {
	cm := testChatModels(t)
	a := New(WithChatModels(cm))
	if err := a.Initialize(context.Background(), adapter.Config{}); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if a.handlers.chatModels != cm {
		t.Fatalf("handlers.chatModels = %v, want the adapter's chat models", a.handlers.chatModels)
	}
}
