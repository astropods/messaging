package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/astropods/messaging/internal/adapter"
	"github.com/astropods/messaging/internal/metrics"
	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
	"github.com/prometheus/client_golang/prometheus/testutil"
	slacklib "github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
)

// mockMessageHandler records messages passed to the handler
type mockMessageHandler struct {
	mu       sync.Mutex
	messages []*pb.Message
}

func (h *mockMessageHandler) handle(ctx context.Context, msg *pb.Message) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.messages = append(h.messages, msg)
	return nil
}

func (h *mockMessageHandler) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.messages)
}

func (h *mockMessageHandler) last() *pb.Message {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.messages) == 0 {
		return nil
	}
	return h.messages[len(h.messages)-1]
}

func newTestAdapter() (*SlackAdapter, *mockMessageHandler) {
	return newTestAdapterWithReactions(nil)
}

func newTestAdapterWithReactions(reactions []string) (*SlackAdapter, *mockMessageHandler) {
	handler := &mockMessageHandler{}
	reactionMap := make(map[string]bool, len(reactions))
	for _, r := range reactions {
		reactionMap[r] = true
	}
	a := &SlackAdapter{
		contentBuffers:      make(map[string]string),
		actionableReactions: reactionMap,
		allowList:           newAllowList(nil, nil, nil), // unrestricted
	}
	a.msgHandler = handler.handle
	return a, handler
}

func TestHandleMessage_DMProcessed(t *testing.T) {
	a, handler := newTestAdapter()

	ev := &slackevents.MessageEvent{
		Channel:   "D123456",
		User:      "U123",
		Text:      "hello",
		TimeStamp: "1234567890.000001",
	}

	beforeEvent := testutil.ToFloat64(metrics.SlackEvents.WithLabelValues("dm"))

	a.handleMessage(t.Context(), ev)

	if handler.count() != 1 {
		t.Fatalf("expected 1 message, got %d", handler.count())
	}
	msg := handler.last()
	if msg.ConversationId != "D123456" {
		t.Errorf("expected conversation ID 'D123456', got %q", msg.ConversationId)
	}
	if got := testutil.ToFloat64(metrics.SlackEvents.WithLabelValues("dm")) - beforeEvent; got != 1 {
		t.Errorf("SlackEvents{dm}: expected +1, got +%v", got)
	}
}

func TestHandleMessage_DMThreadReplyProcessed(t *testing.T) {
	a, handler := newTestAdapter()

	ev := &slackevents.MessageEvent{
		Channel:         "D123456",
		User:            "U123",
		Text:            "follow up",
		TimeStamp:       "1234567891.000001",
		ThreadTimeStamp: "1234567890.000001",
	}

	a.handleMessage(t.Context(), ev)

	if handler.count() != 1 {
		t.Fatalf("expected 1 message, got %d", handler.count())
	}
	msg := handler.last()
	if msg.ConversationId != "D123456-1234567890.000001" {
		t.Errorf("expected conversation ID 'D123456-1234567890.000001', got %q", msg.ConversationId)
	}
}

func TestHandleMessage_ChannelTopLevelIgnored(t *testing.T) {
	a, handler := newTestAdapter()

	ev := &slackevents.MessageEvent{
		Channel:   "C123456",
		User:      "U123",
		Text:      "hello channel",
		TimeStamp: "1234567890.000001",
	}

	a.handleMessage(t.Context(), ev)

	if handler.count() != 0 {
		t.Errorf("expected top-level channel message to be ignored, got %d messages", handler.count())
	}
}

func TestHandleMessage_ChannelThreadReplyProcessed(t *testing.T) {
	a, handler := newTestAdapter()

	ev := &slackevents.MessageEvent{
		Channel:         "C123456",
		User:            "U123",
		Text:            "thread reply without mention",
		TimeStamp:       "1234567891.000001",
		ThreadTimeStamp: "1234567890.000001",
	}

	a.handleMessage(t.Context(), ev)

	if handler.count() != 1 {
		t.Fatalf("expected thread reply in channel to be processed, got %d messages", handler.count())
	}
	msg := handler.last()
	expectedConvID := "C123456-1234567890.000001"
	if msg.ConversationId != expectedConvID {
		t.Errorf("expected conversation ID %q, got %q", expectedConvID, msg.ConversationId)
	}
	if msg.Content != "thread reply without mention" {
		t.Errorf("expected content 'thread reply without mention', got %q", msg.Content)
	}
}

func TestHandleMessage_BotMessageIgnored(t *testing.T) {
	a, handler := newTestAdapter()

	ev := &slackevents.MessageEvent{
		Channel:   "D123456",
		User:      "U123",
		BotID:     "B123",
		Text:      "bot message",
		TimeStamp: "1234567890.000001",
	}

	beforeDropped := testutil.ToFloat64(metrics.MessagesDropped.WithLabelValues("slack", "bot_filtered"))

	a.handleMessage(t.Context(), ev)

	if handler.count() != 0 {
		t.Errorf("expected bot message to be ignored, got %d messages", handler.count())
	}
	if got := testutil.ToFloat64(metrics.MessagesDropped.WithLabelValues("slack", "bot_filtered")) - beforeDropped; got != 1 {
		t.Errorf("MessagesDropped{bot_filtered}: expected +1, got +%v", got)
	}
}

func TestHandleMessage_SubtypeIgnored(t *testing.T) {
	a, handler := newTestAdapter()

	ev := &slackevents.MessageEvent{
		Channel:   "D123456",
		User:      "U123",
		Text:      "edited message",
		SubType:   "message_changed",
		TimeStamp: "1234567890.000001",
	}

	a.handleMessage(t.Context(), ev)

	if handler.count() != 0 {
		t.Errorf("expected message_changed subtype to be ignored, got %d messages", handler.count())
	}
}

func TestHandleMessage_ThreadBroadcastAllowed(t *testing.T) {
	a, handler := newTestAdapter()

	ev := &slackevents.MessageEvent{
		Channel:         "D123456",
		User:            "U123",
		Text:            "broadcast reply",
		SubType:         "thread_broadcast",
		TimeStamp:       "1234567891.000001",
		ThreadTimeStamp: "1234567890.000001",
	}

	a.handleMessage(t.Context(), ev)

	if handler.count() != 1 {
		t.Fatalf("expected thread_broadcast to be processed, got %d messages", handler.count())
	}
}

func TestHandleMessage_PlatformContext(t *testing.T) {
	a, handler := newTestAdapter()

	ev := &slackevents.MessageEvent{
		Channel:         "C123456",
		User:            "U789",
		Text:            "thread msg",
		TimeStamp:       "1234567891.000001",
		ThreadTimeStamp: "1234567890.000001",
	}

	a.handleMessage(t.Context(), ev)

	if handler.count() != 1 {
		t.Fatalf("expected 1 message, got %d", handler.count())
	}
	msg := handler.last()
	if msg.Platform != "slack" {
		t.Errorf("expected platform 'slack', got %q", msg.Platform)
	}
	if msg.PlatformContext.ChannelId != "C123456" {
		t.Errorf("expected channel ID 'C123456', got %q", msg.PlatformContext.ChannelId)
	}
	if msg.PlatformContext.ThreadId != "1234567890.000001" {
		t.Errorf("expected thread ID '1234567890.000001', got %q", msg.PlatformContext.ThreadId)
	}
	if msg.User.Id != "U789" {
		t.Errorf("expected user ID 'U789', got %q", msg.User.Id)
	}
}

func TestHandleMessage_AllowedChannelIDs_DisallowedDoesNotInvokeHandler(t *testing.T) {
	a, handler := newTestAdapter()
	a.allowList = newAllowList(nil, []string{"C999"}, nil) // only C999 is allowed
	srv := newFakeSlackServer(t, "")
	defer srv.Close()
	a.client = slacklib.New("xoxb-fake", slacklib.OptionAPIURL(srv.URL+"/"))

	ev := &slackevents.MessageEvent{
		Channel:         "C123456",
		User:            "U123",
		Text:            "hello",
		TimeStamp:       "1234567891.000001",
		ThreadTimeStamp: "1234567890.000001",
	}

	a.handleMessage(t.Context(), ev)

	if handler.count() != 0 {
		t.Errorf("disallowed event must not invoke msgHandler, got %d messages", handler.count())
	}
}

func TestHandleMessage_AllowedChannelIDs_AllowedInvokesHandler(t *testing.T) {
	a, handler := newTestAdapter()
	a.allowList = newAllowList(nil, []string{"C123456"}, nil) // C123456 is allowed
	srv := newFakeSlackServer(t, "")
	defer srv.Close()
	a.client = slacklib.New("xoxb-fake", slacklib.OptionAPIURL(srv.URL+"/"))

	ev := &slackevents.MessageEvent{
		Channel:         "C123456",
		User:            "U123",
		Text:            "hello",
		TimeStamp:       "1234567891.000001",
		ThreadTimeStamp: "1234567890.000001",
	}

	a.handleMessage(t.Context(), ev)

	if handler.count() != 1 {
		t.Fatalf("allowed event must invoke msgHandler, got %d messages", handler.count())
	}
}

func TestHandleMessage_AdminUserIDs_DisallowedDoesNotInvokeHandler(t *testing.T) {
	a, handler := newTestAdapter()
	a.allowList = newAllowList([]string{"U999"}, nil, nil) // only U999 is admin
	srv := newFakeSlackServer(t, "")
	defer srv.Close()
	a.client = slacklib.New("xoxb-fake", slacklib.OptionAPIURL(srv.URL+"/"))

	ev := &slackevents.MessageEvent{
		Channel:   "D123456",
		User:      "U123", // not in admin list and no dynamic grants
		Text:      "hello dm",
		TimeStamp: "1234567890.000001",
	}

	a.handleMessage(t.Context(), ev)

	if handler.count() != 0 {
		t.Errorf("disallowed event must not invoke msgHandler, got %d messages", handler.count())
	}
}

func TestHandleMessage_AdminUserIDs_AllowedInvokesHandle(t *testing.T) {
	a, handler := newTestAdapter()
	a.allowList = newAllowList([]string{"U123"}, nil, nil) // U123 is admin
	srv := newFakeSlackServer(t, "")
	defer srv.Close()
	a.client = slacklib.New("xoxb-fake", slacklib.OptionAPIURL(srv.URL+"/"))

	ev := &slackevents.MessageEvent{
		Channel:   "D123456",
		User:      "U123",
		Text:      "hello dm",
		TimeStamp: "1234567890.000001",
	}

	a.handleMessage(t.Context(), ev)

	if handler.count() != 1 {
		t.Fatalf("allowed event must invoke msgHandler, got %d messages", handler.count())
	}
}

func TestHandleMessage_DynamicAllowlist_GrantedUserInvokesHandle(t *testing.T) {
	a, handler := newTestAdapter()
	// U999 is admin; U123 is granted dynamic access
	a.allowList = newAllowList([]string{"U999"}, nil, nil)
	if err := a.allowList.addUser(t.Context(), "U123"); err != nil {
		t.Fatalf("addUser: %v", err)
	}
	srv := newFakeSlackServer(t, "")
	defer srv.Close()
	a.client = slacklib.New("xoxb-fake", slacklib.OptionAPIURL(srv.URL+"/"))

	ev := &slackevents.MessageEvent{
		Channel:   "D123456",
		User:      "U123",
		Text:      "hello dm",
		TimeStamp: "1234567890.000001",
	}

	a.handleMessage(t.Context(), ev)

	if handler.count() != 1 {
		t.Fatalf("dynamically granted user must invoke msgHandler, got %d messages", handler.count())
	}
}

func TestHandleAppMention_AllowedChannelIDs_DisallowedDoesNotInvokeHandlerAndPostsNotEnabled(t *testing.T) {
	a, handler := newTestAdapter()
	a.allowList = newAllowList(nil, []string{"C999"}, nil) // only C999 is allowed
	srv := newFakeSlackServer(t, "")
	defer srv.Close()
	a.client = slacklib.New("xoxb-fake", slacklib.OptionAPIURL(srv.URL+"/"))

	ev := &slackevents.AppMentionEvent{
		Channel:         "C123456",
		User:            "U123",
		Text:            "<@BOT> hello",
		TimeStamp:       "1234567890.000001",
		ThreadTimeStamp: "",
	}

	a.handleAppMention(t.Context(), ev)

	if handler.count() != 0 {
		t.Errorf("disallowed app_mention must not invoke msgHandler, got %d messages", handler.count())
	}
}

func TestHandleAppMention_AllowedChannelIDs_AllowedInvokesHandlerAndDoesNotPostNotEnabled(t *testing.T) {
	a, handler := newTestAdapter()
	a.allowList = newAllowList(nil, []string{"C123456"}, nil) // C123456 is allowed
	srv := newFakeSlackServer(t, "")
	defer srv.Close()
	a.client = slacklib.New("xoxb-fake", slacklib.OptionAPIURL(srv.URL+"/"))
	// aiClient is used for SetThreadStatus when allowed; point at fake server so it doesn't panic
	a.aiClient = &SlackAIClient{
		botToken:   "xoxb-fake",
		httpClient: srv.Client(),
		baseURL:    srv.URL,
	}

	ev := &slackevents.AppMentionEvent{
		Channel:         "C123456",
		User:            "U123",
		Text:            "<@BOT> hello",
		TimeStamp:       "1234567890.000001",
		ThreadTimeStamp: "",
	}

	a.handleAppMention(t.Context(), ev)

	if handler.count() != 1 {
		t.Fatalf("allowed app_mention must invoke msgHandler, got %d messages", handler.count())
	}
}

func TestHandleReactionAdded_ActionableReactionForwarded(t *testing.T) {
	a, handler := newTestAdapterWithReactions([]string{"ticket"})
	srv := newFakeSlackServer(t, "original message text")
	defer srv.Close()
	a.client = slacklib.New("xoxb-fake", slacklib.OptionAPIURL(srv.URL+"/"))

	ev := &slackevents.ReactionAddedEvent{
		Reaction: "ticket",
		User:     "U123",
		Item: slackevents.Item{
			Channel:   "C123456",
			Timestamp: "1234567890.000001",
		},
	}

	a.handleReactionAdded(t.Context(), ev)

	if handler.count() != 1 {
		t.Fatalf("expected actionable reaction to be forwarded, got %d messages", handler.count())
	}
	msg := handler.last()
	if msg.PlatformContext.ChannelId != "C123456" {
		t.Errorf("expected channel 'C123456', got %q", msg.PlatformContext.ChannelId)
	}
}

func TestHandleReactionAdded_NonActionableReactionDropped(t *testing.T) {
	a, handler := newTestAdapterWithReactions([]string{"ticket"})

	ev := &slackevents.ReactionAddedEvent{
		Reaction: "thumbsup",
		User:     "U123",
		Item: slackevents.Item{
			Channel:   "C123456",
			Timestamp: "1234567890.000001",
		},
	}

	a.handleReactionAdded(t.Context(), ev)

	if handler.count() != 0 {
		t.Errorf("expected non-actionable reaction to be dropped, got %d messages", handler.count())
	}
}

func TestHandleReactionAdded_EmptyMapDropsAll(t *testing.T) {
	a, handler := newTestAdapterWithReactions(nil)

	ev := &slackevents.ReactionAddedEvent{
		Reaction: "ticket",
		User:     "U123",
		Item: slackevents.Item{
			Channel:   "C123456",
			Timestamp: "1234567890.000001",
		},
	}

	a.handleReactionAdded(t.Context(), ev)

	if handler.count() != 0 {
		t.Errorf("expected all reactions dropped when no actionable reactions configured, got %d", handler.count())
	}
}

func TestSendErrorMessage_SuppressesInfraError(t *testing.T) {
	srv := newFakeSlackServer(t, "")
	defer srv.Close()
	a := &SlackAdapter{
		client:         slacklib.New("xoxb-fake", slacklib.OptionAPIURL(srv.URL+"/")),
		contentBuffers: make(map[string]string),
	}

	a.sendErrorMessage(t.Context(), "C123", "1234.0001", adapter.ErrNoAgentStream)

	if srv.postCount > 0 {
		t.Error("expected ErrNoAgentStream to be suppressed, but a message was posted")
	}
}

func TestSendErrorMessage_SuppressesWrappedInfraError(t *testing.T) {
	srv := newFakeSlackServer(t, "")
	defer srv.Close()
	a := &SlackAdapter{
		client:         slacklib.New("xoxb-fake", slacklib.OptionAPIURL(srv.URL+"/")),
		contentBuffers: make(map[string]string),
	}

	wrapped := fmt.Errorf("%w for conversation: conv-123", adapter.ErrNoAgentStream)
	a.sendErrorMessage(t.Context(), "C123", "1234.0001", wrapped)

	if srv.postCount > 0 {
		t.Error("expected wrapped ErrNoAgentStream to be suppressed, but a message was posted")
	}
}

func TestSendErrorMessage_PostsUserFacingError(t *testing.T) {
	srv := newFakeSlackServer(t, "")
	defer srv.Close()
	a := &SlackAdapter{
		client:         slacklib.New("xoxb-fake", slacklib.OptionAPIURL(srv.URL+"/")),
		contentBuffers: make(map[string]string),
	}

	a.sendErrorMessage(t.Context(), "C123", "1234.0001", fmt.Errorf("tool execution failed"))

	if srv.postCount != 1 {
		t.Errorf("expected user-facing error to be posted, got %d messages", srv.postCount)
	}
}

func TestInitialize_ActionableReactionsFromConfig(t *testing.T) {
	a := &SlackAdapter{contentBuffers: make(map[string]string)}
	cfg := adapter.Config{
		BotToken:            "xoxb-test",
		AppToken:            "xapp-test",
		SocketMode:          false,
		AutoThread:          true,
		ActionableReactions: []string{"ticket", "bug"},
		RateLimit:           adapter.RateLimitConfig{RequestsPerSecond: 1, BurstSize: 1},
	}

	err := a.Initialize(t.Context(), cfg)
	if err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}

	if len(a.actionableReactions) != 2 {
		t.Fatalf("actionableReactions len = %d, want 2", len(a.actionableReactions))
	}
	if !a.actionableReactions["ticket"] {
		t.Error("expected 'ticket' in actionableReactions")
	}
	if !a.actionableReactions["bug"] {
		t.Error("expected 'bug' in actionableReactions")
	}
}

func TestInitialize_EmptyReactionsDropsAll(t *testing.T) {
	a := &SlackAdapter{contentBuffers: make(map[string]string)}
	cfg := adapter.Config{
		BotToken:   "xoxb-test",
		AppToken:   "xapp-test",
		SocketMode: false,
		RateLimit:  adapter.RateLimitConfig{RequestsPerSecond: 1, BurstSize: 1},
	}

	err := a.Initialize(t.Context(), cfg)
	if err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}

	if len(a.actionableReactions) != 0 {
		t.Errorf("actionableReactions should be empty, got %v", a.actionableReactions)
	}
}

func TestInitialize_SocketModeConfig(t *testing.T) {
	a := &SlackAdapter{contentBuffers: make(map[string]string)}
	cfg := adapter.Config{
		BotToken:   "xoxb-test",
		AppToken:   "xapp-test",
		SocketMode: true,
		RateLimit:  adapter.RateLimitConfig{RequestsPerSecond: 1, BurstSize: 1},
	}

	err := a.Initialize(t.Context(), cfg)
	if err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}

	if !a.config.SocketMode {
		t.Error("expected SocketMode=true in stored config")
	}
	if a.socketClient == nil {
		t.Error("expected socketClient to be initialized when SocketMode=true")
	}
}

func TestInitialize_SocketModeDisabled(t *testing.T) {
	a := &SlackAdapter{contentBuffers: make(map[string]string)}
	cfg := adapter.Config{
		BotToken:   "xoxb-test",
		AppToken:   "xapp-test",
		SocketMode: false,
		RateLimit:  adapter.RateLimitConfig{RequestsPerSecond: 1, BurstSize: 1},
	}

	err := a.Initialize(t.Context(), cfg)
	if err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}

	if a.config.SocketMode {
		t.Error("expected SocketMode=false in stored config")
	}
	if a.socketClient != nil {
		t.Error("expected socketClient to be nil when SocketMode=false")
	}
}

func TestInitialize_AutoThreadConfig(t *testing.T) {
	a := &SlackAdapter{contentBuffers: make(map[string]string)}
	cfg := adapter.Config{
		BotToken:   "xoxb-test",
		AppToken:   "xapp-test",
		SocketMode: false,
		AutoThread: true,
		RateLimit:  adapter.RateLimitConfig{RequestsPerSecond: 1, BurstSize: 1},
	}

	err := a.Initialize(t.Context(), cfg)
	if err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}

	if !a.config.AutoThread {
		t.Error("expected AutoThread=true in stored config")
	}
}

// fakeSlackServer is an httptest server that stubs the Slack API endpoints
// needed by tests. It records calls to chat.postMessage.
type fakeSlackServer struct {
	*httptest.Server
	postCount int
}

func newFakeSlackServer(t *testing.T, replyText string) *fakeSlackServer {
	t.Helper()
	fs := &fakeSlackServer{}

	mux := http.NewServeMux()

	mux.HandleFunc("/conversations.replies", func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]interface{}{
			"ok": true,
			"messages": []map[string]interface{}{
				{"ts": r.FormValue("ts"), "text": replyText, "user": "U999"},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})

	mux.HandleFunc("/chat.postMessage", func(w http.ResponseWriter, r *http.Request) {
		fs.postCount++
		resp := map[string]interface{}{
			"ok":      true,
			"channel": r.FormValue("channel"),
			"ts":      "1234567890.000099",
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
	})

	fs.Server = httptest.NewServer(mux)
	return fs
}
