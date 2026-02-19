package slack

import (
	"context"
	"sync"
	"testing"

	pb "github.com/astromode-ai/astro-messaging/pkg/gen/astro/messaging/v1"
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
	handler := &mockMessageHandler{}
	a := &SlackAdapter{
		contentBuffers: make(map[string]string),
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

	a.handleMessage(t.Context(), ev)

	if handler.count() != 1 {
		t.Fatalf("expected 1 message, got %d", handler.count())
	}
	msg := handler.last()
	if msg.ConversationId != "D123456" {
		t.Errorf("expected conversation ID 'D123456', got %q", msg.ConversationId)
	}
}

func TestHandleMessage_DMThreadReplyProcessed(t *testing.T) {
	a, handler := newTestAdapter()

	ev := &slackevents.MessageEvent{
		Channel:        "D123456",
		User:           "U123",
		Text:           "follow up",
		TimeStamp:      "1234567891.000001",
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
		Channel:        "C123456",
		User:           "U123",
		Text:           "thread reply without mention",
		TimeStamp:      "1234567891.000001",
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

	a.handleMessage(t.Context(), ev)

	if handler.count() != 0 {
		t.Errorf("expected bot message to be ignored, got %d messages", handler.count())
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
		Channel:        "D123456",
		User:           "U123",
		Text:           "broadcast reply",
		SubType:        "thread_broadcast",
		TimeStamp:      "1234567891.000001",
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
		Channel:        "C123456",
		User:           "U789",
		Text:           "thread msg",
		TimeStamp:      "1234567891.000001",
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
