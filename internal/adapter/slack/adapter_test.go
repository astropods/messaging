package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/astropods/messaging/internal/adapter"
	"github.com/astropods/messaging/internal/authz"
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

	a.handleMessage(t.Context(), ev, "")

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

	a.handleMessage(t.Context(), ev, "")

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

	a.handleMessage(t.Context(), ev, "")

	if handler.count() != 0 {
		t.Errorf("expected top-level channel message to be ignored, got %d messages", handler.count())
	}
}

func TestHandleMessage_ObserveChannel_TopLevelForwarded(t *testing.T) {
	a, handler := newTestAdapter()
	a.observeChannels = map[string]bool{"C123456": true}
	a.msgDedup = newSlackMsgDedup(8)
	a.botUserID = "UBOTTEST"

	ev := &slackevents.MessageEvent{
		Channel:   "C123456",
		User:      "U123",
		Text:      "hello everyone",
		TimeStamp: "9999999999.000001",
	}
	a.handleMessage(t.Context(), ev, "")

	if handler.count() != 1 {
		t.Fatalf("expected observed top-level to forward, got %d", handler.count())
	}
	msg := handler.last()
	if msg.PlatformContext.ThreadId != "9999999999.000001" {
		t.Errorf("expected ThreadId == TimeStamp, got %q", msg.PlatformContext.ThreadId)
	}
	if msg.ConversationId != "C123456-9999999999.000001" {
		t.Errorf("expected conv id 'C123456-9999999999.000001', got %q", msg.ConversationId)
	}
	if msg.Content != "hello everyone" {
		t.Errorf("expected raw text, got %q", msg.Content)
	}
	if msg.PlatformContext.EventKind != pb.PlatformContext_EVENT_KIND_OBSERVED {
		t.Errorf("expected EventKind=EVENT_KIND_OBSERVED, got %v", msg.PlatformContext.EventKind)
	}
	if msg.PlatformContext.ThreadRootId != "" {
		t.Errorf("expected ThreadRootId empty for top-level observed, got %q", msg.PlatformContext.ThreadRootId)
	}
	if msg.PlatformContext.BotUserId != "UBOTTEST" {
		t.Errorf("expected BotUserId='UBOTTEST', got %q", msg.PlatformContext.BotUserId)
	}
}

func TestHandleMessage_ObserveChannel_BotMentionDropped(t *testing.T) {
	a, handler := newTestAdapter()
	a.observeChannels = map[string]bool{"C123456": true}
	a.botUserID = "UBOTTEST"
	a.msgDedup = newSlackMsgDedup(8)
	before := testutil.ToFloat64(metrics.MessagesDropped.WithLabelValues("slack", "app_mention_dedup"))

	ev := &slackevents.MessageEvent{
		Channel:   "C123456",
		User:      "U123",
		Text:      "<@UBOTTEST> please help",
		TimeStamp: "8888888888.000001",
	}
	a.handleMessage(t.Context(), ev, "")

	if handler.count() != 0 {
		t.Fatalf("expected bot-mention text to be skipped (app_mention will handle), got %d", handler.count())
	}
	if got := testutil.ToFloat64(metrics.MessagesDropped.WithLabelValues("slack", "app_mention_dedup")) - before; got != 1 {
		t.Errorf("expected app_mention_dedup +1, got +%v", got)
	}
}

func TestHandleMessage_ObserveChannel_DuplicateSuppressed(t *testing.T) {
	a, handler := newTestAdapter()
	a.observeChannels = map[string]bool{"C123456": true}
	a.msgDedup = newSlackMsgDedup(8)

	ev := &slackevents.MessageEvent{
		Channel:   "C123456",
		User:      "U123",
		Text:      "hi",
		TimeStamp: "7777777777.000001",
	}
	a.handleMessage(t.Context(), ev, "")
	a.handleMessage(t.Context(), ev, "")

	if handler.count() != 1 {
		t.Fatalf("expected dedup to drop second delivery, got %d", handler.count())
	}
}

func TestHandleMessage_ChannelThreadReplyProcessed(t *testing.T) {
	a, handler := newTestAdapter()
	a.botUserID = "UBOTTEST"

	ev := &slackevents.MessageEvent{
		Channel:         "C123456",
		User:            "U123",
		Text:            "thread reply without mention",
		TimeStamp:       "1234567891.000001",
		ThreadTimeStamp: "1234567890.000001",
	}

	a.handleMessage(t.Context(), ev, "")

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
	if msg.PlatformContext.EventKind != pb.PlatformContext_EVENT_KIND_THREAD_REPLY {
		t.Errorf("expected EventKind=EVENT_KIND_THREAD_REPLY, got %v", msg.PlatformContext.EventKind)
	}
	if msg.PlatformContext.ThreadRootId != "1234567890.000001" {
		t.Errorf("expected ThreadRootId='1234567890.000001', got %q", msg.PlatformContext.ThreadRootId)
	}
	if msg.PlatformContext.BotUserId != "UBOTTEST" {
		t.Errorf("expected BotUserId='UBOTTEST', got %q", msg.PlatformContext.BotUserId)
	}
}

// EVENT_KIND_DM is set on every direct-message ingress (top-level and thread).
// ThreadRootId is empty on a top-level DM, non-empty on a reply.
func TestHandleMessage_DM_EventKindAndThreadRoot(t *testing.T) {
	t.Run("top-level", func(t *testing.T) {
		a, handler := newTestAdapter()
		ev := &slackevents.MessageEvent{
			Channel: "D123", User: "U1", Text: "hi", TimeStamp: "11.000001",
		}
		a.handleMessage(t.Context(), ev, "")
		if handler.count() != 1 {
			t.Fatalf("expected forward, got %d", handler.count())
		}
		pc := handler.last().PlatformContext
		if pc.EventKind != pb.PlatformContext_EVENT_KIND_DM {
			t.Errorf("expected EVENT_KIND_DM, got %v", pc.EventKind)
		}
		if pc.ThreadRootId != "" {
			t.Errorf("expected empty ThreadRootId for top-level DM, got %q", pc.ThreadRootId)
		}
	})

	t.Run("thread reply", func(t *testing.T) {
		a, handler := newTestAdapter()
		ev := &slackevents.MessageEvent{
			Channel: "D123", User: "U1", Text: "follow-up",
			TimeStamp: "12.000001", ThreadTimeStamp: "11.000001",
		}
		a.handleMessage(t.Context(), ev, "")
		if handler.count() != 1 {
			t.Fatalf("expected forward, got %d", handler.count())
		}
		pc := handler.last().PlatformContext
		if pc.EventKind != pb.PlatformContext_EVENT_KIND_DM {
			t.Errorf("expected EVENT_KIND_DM, got %v", pc.EventKind)
		}
		if pc.ThreadRootId != "11.000001" {
			t.Errorf("expected ThreadRootId='11.000001', got %q", pc.ThreadRootId)
		}
	})
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

	a.handleMessage(t.Context(), ev, "")

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

	a.handleMessage(t.Context(), ev, "")

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

	a.handleMessage(t.Context(), ev, "")

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

	a.handleMessage(t.Context(), ev, "")

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
	a.config = adapter.Config{AllowedChannelIDs: []string{"C999"}}
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

	a.handleMessage(t.Context(), ev, "")

	if handler.count() != 0 {
		t.Errorf("disallowed event must not invoke msgHandler, got %d messages", handler.count())
	}
}

func TestHandleMessage_AllowedChannelIDs_AllowedInvokesHandler(t *testing.T) {
	a, handler := newTestAdapter()
	a.config = adapter.Config{AllowedChannelIDs: []string{"C123456"}}
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

	a.handleMessage(t.Context(), ev, "")

	if handler.count() != 1 {
		t.Fatalf("allowed event must invoke msgHandler, got %d messages", handler.count())
	}
}

func TestHandleMessage_AllowedUserIDs_DisallowedDoesNotInvokeHandler(t *testing.T) {
	a, handler := newTestAdapter()
	a.config = adapter.Config{AllowedUserIDs: []string{"U999"}}
	srv := newFakeSlackServer(t, "")
	defer srv.Close()
	a.client = slacklib.New("xoxb-fake", slacklib.OptionAPIURL(srv.URL+"/"))

	ev := &slackevents.MessageEvent{
		Channel:   "D123456",
		User:      "U123",
		Text:      "hello dm",
		TimeStamp: "1234567890.000001",
	}

	a.handleMessage(t.Context(), ev, "")

	if handler.count() != 0 {
		t.Errorf("disallowed event must not invoke msgHandler, got %d messages", handler.count())
	}
}

func TestHandleMessage_AllowedUserIDs_AllowedInvokesHandle(t *testing.T) {
	a, handler := newTestAdapter()
	a.config = adapter.Config{AllowedUserIDs: []string{"U123"}}
	srv := newFakeSlackServer(t, "")
	defer srv.Close()
	a.client = slacklib.New("xoxb-fake", slacklib.OptionAPIURL(srv.URL+"/"))

	ev := &slackevents.MessageEvent{
		Channel:   "D123456",
		User:      "U123",
		Text:      "hello dm",
		TimeStamp: "1234567890.000001",
	}

	a.handleMessage(t.Context(), ev, "")

	if handler.count() != 1 {
		t.Fatalf("allowed event must invoke msgHandler, got %d messages", handler.count())
	}
}

func TestHandleAppMention_AllowedChannelIDs_DisallowedDoesNotInvokeHandlerAndPostsNotEnabled(t *testing.T) {
	a, handler := newTestAdapter()
	a.config = adapter.Config{AllowedChannelIDs: []string{"C999"}}
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

	a.handleAppMention(t.Context(), ev, "")

	if handler.count() != 0 {
		t.Errorf("disallowed app_mention must not invoke msgHandler, got %d messages", handler.count())
	}
}

func TestHandleAppMention_AllowedChannelIDs_AllowedInvokesHandlerAndDoesNotPostNotEnabled(t *testing.T) {
	a, handler := newTestAdapter()
	a.config = adapter.Config{AllowedChannelIDs: []string{"C123456"}}
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

	a.handleAppMention(t.Context(), ev, "")

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

	a.handleReactionAdded(t.Context(), ev, "")

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

	a.handleReactionAdded(t.Context(), ev, "")

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

	a.handleReactionAdded(t.Context(), ev, "")

	if handler.count() != 0 {
		t.Errorf("expected all reactions dropped when no actionable reactions configured, got %d", handler.count())
	}
}

// errAuthorizer always returns a transport error. Mirrors a real-world
// failure where the authz endpoint is unreachable or returns 5xx.
type errAuthorizer struct {
	calls int
	err   error
}

func (e *errAuthorizer) Authorize(_ context.Context, _, _, _, _ string) (authz.Result, error) {
	e.calls++
	return authz.Result{}, e.err
}

// dispatch must return errAuthzUnavailable on authz transport error. The
// sentinel lets callers post a sanitized user-facing reply via
// sendErrorMessage; the raw authz error is never propagated to slack.
func TestDispatch_AuthzTransportError_ReturnsUnavailableSentinel(t *testing.T) {
	a, handler := newTestAdapter()
	az := &errAuthorizer{err: fmt.Errorf("authz endpoint unreachable")}
	a.SetAuthorizer(az)

	msg := &pb.Message{User: &pb.User{Id: "U123"}}

	err := a.dispatch(t.Context(), msg, "")
	if !errors.Is(err, errAuthzUnavailable) {
		t.Errorf("expected errAuthzUnavailable, got %v", err)
	}
	if az.calls != 1 {
		t.Errorf("expected exactly one Allowed() call, got %d", az.calls)
	}
	if handler.count() != 0 {
		t.Errorf("msgHandler should not be invoked on authz transport error; got %d call(s)", handler.count())
	}
}

// denyAuthorizer always returns allowed=false with no error.
type denyAuthorizer struct{ calls int }

func (d *denyAuthorizer) Authorize(_ context.Context, _, _, _, _ string) (authz.Result, error) {
	d.calls++
	return authz.Result{Allowed: false}, nil
}

func TestDispatch_AuthzDenied_ReturnsDeniedSentinel(t *testing.T) {
	a, handler := newTestAdapter()
	az := &denyAuthorizer{}
	a.SetAuthorizer(az)

	msg := &pb.Message{User: &pb.User{Id: "U123"}}

	err := a.dispatch(t.Context(), msg, "")
	if !errors.Is(err, errAuthzDenied) {
		t.Errorf("expected errAuthzDenied, got %v", err)
	}
	if handler.count() != 0 {
		t.Errorf("msgHandler should not be invoked on deny; got %d call(s)", handler.count())
	}
}

// Observe channels are passive watch channels — the user didn't address the
// bot, so dispatch must skip the per-user authz check (and the identity
// rewrite, which would misattribute the trace).
func TestDispatch_ObserveChannel_SkipsAuthz(t *testing.T) {
	a, handler := newTestAdapter()
	a.observeChannels = map[string]bool{"C123456": true}
	az := &denyAuthorizer{}
	a.SetAuthorizer(az)

	msg := &pb.Message{
		User:            &pb.User{Id: "U123"},
		PlatformContext: &pb.PlatformContext{ChannelId: "C123456"},
	}

	if err := a.dispatch(t.Context(), msg, ""); err != nil {
		t.Fatalf("expected nil err for observed message, got %v", err)
	}
	if az.calls != 0 {
		t.Errorf("authorizer should not be called for observe channel, got %d call(s)", az.calls)
	}
	if handler.count() != 1 {
		t.Errorf("msgHandler should be invoked for observed message; got %d", handler.count())
	}
	if msg.User.Id != "U123" {
		t.Errorf("msg.User.Id should not be rewritten for observed message; got %q", msg.User.Id)
	}
}

// stubAuthorizer returns a canned Result so we can assert that dispatch
// rewrites pb.Message.User.Id based on the resolved identity. This is the
// core of the Slack→WorkOS attribution round-trip.
type stubAuthorizer struct {
	result authz.Result
}

func (s *stubAuthorizer) Authorize(_ context.Context, _, _, _, _ string) (authz.Result, error) {
	return s.result, nil
}

// Linked Slack user → dispatch rewrites pb.Message.User.Id to the canonical
// WorkOS user_id so Langfuse traces carry the user's Astro identity.
func TestDispatch_LinkedSlack_RewritesToWorkOSUserID(t *testing.T) {
	a, handler := newTestAdapter()
	a.SetAuthorizer(&stubAuthorizer{result: authz.Result{
		Allowed: true,
		UserID:  "user_alice",
	}})

	msg := &pb.Message{User: &pb.User{Id: "U07ABCDEF"}}
	if err := a.dispatch(t.Context(), msg, "T07XYZ"); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if handler.count() != 1 {
		t.Fatalf("expected msgHandler called once, got %d", handler.count())
	}
	if msg.User.Id != "user_alice" {
		t.Errorf("expected canonical user_id rewrite, got %q", msg.User.Id)
	}
}

// Unlinked Slack user → dispatch keeps the raw slack id on msg.User.Id.
// Same format as every historical Langfuse trace, so the Insights "by
// people" view aggregates pre-PR and post-PR traffic from the same human
// into one row (no duplicates). astro-server's directory join attaches
// team_id separately for the slack:// deep link.
func TestDispatch_UnlinkedSlack_KeepsRawSlackID(t *testing.T) {
	a, handler := newTestAdapter()
	a.SetAuthorizer(&stubAuthorizer{result: authz.Result{Allowed: true}})

	msg := &pb.Message{User: &pb.User{Id: "U07ABCDEF"}}
	if err := a.dispatch(t.Context(), msg, "T07XYZ"); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if handler.count() != 1 {
		t.Fatalf("expected msgHandler called once, got %d", handler.count())
	}
	if want := "U07ABCDEF"; msg.User.Id != want {
		t.Errorf("expected raw slack id %q, got %q", want, msg.User.Id)
	}
}

// Degraded-mode fallback (server unreachable, anyone-adapters claim): the
// Authorizer returns Allowed=true with empty identity fields. The adapter
// keeps the raw slack id — same as the normal unlinked path — so the trace
// still attributes to that user.
func TestDispatch_DegradedMode_FallsBackToRawSlackID(t *testing.T) {
	a, handler := newTestAdapter()
	a.SetAuthorizer(&stubAuthorizer{result: authz.Result{Allowed: true}})

	msg := &pb.Message{User: &pb.User{Id: "U07ABCDEF"}}
	if err := a.dispatch(t.Context(), msg, "T07XYZ"); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if handler.count() != 1 {
		t.Fatalf("expected msgHandler called once, got %d", handler.count())
	}
	if want := "U07ABCDEF"; msg.User.Id != want {
		t.Errorf("expected raw slack id in degraded mode %q, got %q", want, msg.User.Id)
	}
}

// Linked Slack user → dispatch preserves the raw Slack user id on
// PlatformContext.UserId even though Message.user.id is rewritten to the
// Astro user ID. Consumers that need to call back into Slack (mentions,
// DMs, lookups) rely on this field.
func TestDispatch_LinkedSlack_PreservesPlatformContextUserID(t *testing.T) {
	a, _ := newTestAdapter()
	a.SetAuthorizer(&stubAuthorizer{result: authz.Result{
		Allowed: true,
		UserID:  "user_alice",
	}})

	msg := &pb.Message{
		User:            &pb.User{Id: "U07ABCDEF"},
		PlatformContext: &pb.PlatformContext{ChannelId: "C123"},
	}
	if err := a.dispatch(t.Context(), msg, "T07XYZ"); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if msg.User.Id != "user_alice" {
		t.Errorf("expected canonical user_id rewrite, got %q", msg.User.Id)
	}
	if got, want := msg.PlatformContext.UserId, "U07ABCDEF"; got != want {
		t.Errorf("PlatformContext.PlatformUserId: want %q, got %q", want, got)
	}
}

// Observe-channel messages skip authz, but PlatformContext.UserId is still
// set so downstream consumers have a uniform place to read the raw Slack
// user id across all ingress paths.
func TestDispatch_ObserveChannel_PreservesPlatformContextUserID(t *testing.T) {
	a, _ := newTestAdapter()
	a.observeChannels = map[string]bool{"C123456": true}
	a.SetAuthorizer(&denyAuthorizer{})

	msg := &pb.Message{
		User:            &pb.User{Id: "U07ABCDEF"},
		PlatformContext: &pb.PlatformContext{ChannelId: "C123456"},
	}
	if err := a.dispatch(t.Context(), msg, ""); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if got, want := msg.PlatformContext.UserId, "U07ABCDEF"; got != want {
		t.Errorf("PlatformContext.PlatformUserId: want %q, got %q", want, got)
	}
}

// TestCanonicalUserID pins both branches of the helper directly. Wire
// format matters: the bare slack id is the same shape every historical
// Langfuse trace already carries, so picking a different format here
// would re-introduce the dual-key duplication problem in Insights (one
// row per historical bare key + a second row per any other key for the
// same human). astro-server's directory join attaches team_id to bare
// ids via slack_identity_mappings — no team needs to live in user_id
// itself.
func TestCanonicalUserID(t *testing.T) {
	cases := []struct {
		name        string
		result      authz.Result
		slackUserID string
		want        string
	}{
		{
			name:        "linked user → WorkOS id wins",
			result:      authz.Result{Allowed: true, UserID: "user_alice"},
			slackUserID: "U07ABCDEF",
			want:        "user_alice",
		},
		{
			name:        "unlinked user → raw slack id (matches historical Langfuse format)",
			result:      authz.Result{Allowed: true},
			slackUserID: "U07ABCDEF",
			want:        "U07ABCDEF",
		},
		{
			name:        "degraded mode (empty result) → still raw slack id",
			result:      authz.Result{Allowed: true},
			slackUserID: "U07ABCDEF",
			want:        "U07ABCDEF",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := canonicalUserID(tc.result, tc.slackUserID)
			if got != tc.want {
				t.Errorf("canonicalUserID(%+v, %q) = %q, want %q",
					tc.result, tc.slackUserID, got, tc.want)
			}
		})
	}
}

func TestSendErrorMessage_AuthzDenied_PostsSanitized(t *testing.T) {
	srv := newFakeSlackServer(t, "")
	defer srv.Close()
	a := &SlackAdapter{
		client:         slacklib.New("xoxb-fake", slacklib.OptionAPIURL(srv.URL+"/")),
		contentBuffers: make(map[string]string),
	}

	a.sendErrorMessage(t.Context(), "C123", "1234.0001", errAuthzDenied)

	if srv.postCount != 1 {
		t.Fatalf("expected exactly one post, got %d", srv.postCount)
	}
	text := srv.postedTexts[0]
	if !strings.Contains(text, "not authorized") {
		t.Errorf("expected sanitized denied text, got %q", text)
	}
	if strings.Contains(text, "authz") {
		t.Errorf("posted text leaks internal term 'authz': %q", text)
	}
}

func TestSendErrorMessage_AuthzUnavailable_PostsSanitized(t *testing.T) {
	srv := newFakeSlackServer(t, "")
	defer srv.Close()
	a := &SlackAdapter{
		client:         slacklib.New("xoxb-fake", slacklib.OptionAPIURL(srv.URL+"/")),
		contentBuffers: make(map[string]string),
	}

	a.sendErrorMessage(t.Context(), "C123", "1234.0001", errAuthzUnavailable)

	if srv.postCount != 1 {
		t.Fatalf("expected exactly one post, got %d", srv.postCount)
	}
	text := srv.postedTexts[0]
	if !strings.Contains(text, "try again") {
		t.Errorf("expected sanitized unavailable text, got %q", text)
	}
	if strings.Contains(text, "authz") {
		t.Errorf("posted text leaks internal term 'authz': %q", text)
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
	postCount   int
	postedTexts []string
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
		fs.postedTexts = append(fs.postedTexts, r.FormValue("text"))
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

// TestHandleMessage_BlockKitContentReachesAgent verifies that block-only
// content (header + section + fields) posted via an app shows up in
// pb.Message.Content alongside the fallback text.
func TestHandleMessage_BlockKitContentReachesAgent(t *testing.T) {
	a, handler := newTestAdapter()

	blocks := blocksFromJSON(t, `[
		{"type":"header","text":{"type":"plain_text","text":"Deploy Status"}},
		{"type":"section",
		 "text":{"type":"mrkdwn","text":"All green."},
		 "fields":[
			{"type":"mrkdwn","text":"*Service:* api"},
			{"type":"mrkdwn","text":"*Version:* v1.2.3"}
		 ]}
	]`)

	ev := &slackevents.MessageEvent{
		Channel:   "D123456",
		User:      "U123",
		Text:      "summary fallback",
		TimeStamp: "1234567890.000001",
		Blocks:    blocks,
	}

	a.handleMessage(t.Context(), ev, "T1")

	if handler.count() != 1 {
		t.Fatalf("expected 1 message, got %d", handler.count())
	}
	got := handler.last().Content
	for _, want := range []string{"summary fallback", "Deploy Status", "All green.", "*Service:* api", "*Version:* v1.2.3"} {
		if !strings.Contains(got, want) {
			t.Errorf("content %q missing %q", got, want)
		}
	}
}

// TestHandleMessage_UserRichTextNotDuplicated verifies that for a
// user-typed message — where Slack auto-derives text from rich_text —
// the agent receives a single un-duplicated rendering.
func TestHandleMessage_UserRichTextNotDuplicated(t *testing.T) {
	a, handler := newTestAdapter()

	blocks := blocksFromJSON(t, `[
		{"type":"rich_text","elements":[
			{"type":"rich_text_section","elements":[
				{"type":"text","text":"hello world"}
			]}
		]}
	]`)

	ev := &slackevents.MessageEvent{
		Channel:   "D123456",
		User:      "U123",
		Text:      "hello world",
		TimeStamp: "1234567890.000001",
		Blocks:    blocks,
	}

	a.handleMessage(t.Context(), ev, "T1")

	if handler.count() != 1 {
		t.Fatalf("expected 1 message, got %d", handler.count())
	}
	if got := handler.last().Content; got != "hello world" {
		t.Errorf("got %q, want %q", got, "hello world")
	}
}

// TestHandleAppMention_BlockKitMergedAndMentionsStripped verifies that
// a section block carrying real content is merged in, and any bot
// mention (in text or rendered from a rich_text user element) is
// stripped from the combined string.
func TestHandleAppMention_BlockKitMergedAndMentionsStripped(t *testing.T) {
	a, handler := newTestAdapter()
	srv := newFakeSlackServer(t, "")
	defer srv.Close()
	a.client = slacklib.New("xoxb-fake", slacklib.OptionAPIURL(srv.URL+"/"))
	a.aiClient = &SlackAIClient{
		botToken:   "xoxb-fake",
		httpClient: srv.Client(),
		baseURL:    srv.URL,
	}

	blocks := blocksFromJSON(t, `[
		{"type":"rich_text","elements":[
			{"type":"rich_text_section","elements":[
				{"type":"user","user_id":"UBOT"},
				{"type":"text","text":" please summarize this report"}
			]}
		]},
		{"type":"section","text":{"type":"mrkdwn","text":"Q3 revenue: $5M"}}
	]`)

	ev := &slackevents.AppMentionEvent{
		Channel:   "C123456",
		User:      "U123",
		Text:      "<@UBOT> please summarize this report",
		TimeStamp: "1234567890.000001",
		Blocks:    blocks,
	}

	a.handleAppMention(t.Context(), ev, "T1")

	if handler.count() != 1 {
		t.Fatalf("expected 1 message, got %d", handler.count())
	}
	got := handler.last().Content
	if strings.Contains(got, "<@UBOT>") {
		t.Errorf("expected bot mention stripped, got %q", got)
	}
	if !strings.Contains(got, "please summarize this report") {
		t.Errorf("expected mention text to survive, got %q", got)
	}
	if !strings.Contains(got, "Q3 revenue: $5M") {
		t.Errorf("expected section text included, got %q", got)
	}
}

// TestHandleMessage_NoBlocksPreservesText guards the common case where
// no blocks are present: behavior must match pre-renderBlocks exactly.
func TestHandleMessage_NoBlocksPreservesText(t *testing.T) {
	a, handler := newTestAdapter()

	ev := &slackevents.MessageEvent{
		Channel:   "D123456",
		User:      "U123",
		Text:      "plain text only",
		TimeStamp: "1234567890.000001",
	}

	a.handleMessage(t.Context(), ev, "T1")

	if handler.count() != 1 {
		t.Fatalf("expected 1 message, got %d", handler.count())
	}
	if got := handler.last().Content; got != "plain text only" {
		t.Errorf("got %q, want %q", got, "plain text only")
	}
}

// --- Tests for feedback handlers ---

// captureFeedbackHandler records PlatformFeedback events for assertion.
type captureFeedbackHandler struct {
	mu    sync.Mutex
	calls []*pb.PlatformFeedback
}

func (c *captureFeedbackHandler) handle(_ context.Context, fb *pb.PlatformFeedback) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, fb)
	return nil
}

func (c *captureFeedbackHandler) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

func (c *captureFeedbackHandler) last() *pb.PlatformFeedback {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.calls) == 0 {
		return nil
	}
	return c.calls[len(c.calls)-1]
}

func TestHandleFeedbackButton_ThumbsUpForwardedToHandler(t *testing.T) {
	a, _ := newTestAdapter()
	srv := newFakeSlackServer(t, "")
	defer srv.Close()
	a.client = slacklib.New("xoxb-fake", slacklib.OptionAPIURL(srv.URL+"/"))

	fbHandler := &captureFeedbackHandler{}
	a.feedbackHandler = fbHandler.handle

	cb := &slacklib.InteractionCallback{
		User:      slacklib.User{ID: "U999", Name: "alice"},
		Channel:   slacklib.Channel{GroupConversation: slacklib.GroupConversation{Conversation: slacklib.Conversation{ID: "C123"}}},
		Container: slacklib.Container{ThreadTs: "1700000000.000001"},
	}
	cb.Message.Timestamp = "1700000000.000002"

	action := &slacklib.BlockAction{ActionID: feedbackButtonsActionID, Value: "positive_feedback"}
	a.handleFeedbackButton(t.Context(), cb, action)

	if fbHandler.count() != 1 {
		t.Fatalf("expected 1 forwarded feedback, got %d", fbHandler.count())
	}
	fb := fbHandler.last()
	if fb.ConversationId != "C123-1700000000.000001" {
		t.Errorf("ConversationId: expected 'C123-1700000000.000001', got %q", fb.ConversationId)
	}
	if fb.ResponseId != "1700000000.000002" {
		t.Errorf("ResponseId: expected '1700000000.000002', got %q", fb.ResponseId)
	}
	if fb.User == nil || fb.User.Id != "U999" || fb.User.Username != "alice" {
		t.Errorf("User: expected {U999, alice}, got %+v", fb.User)
	}
	react := fb.GetReaction()
	if react == nil {
		t.Fatal("expected Reaction variant")
	}
	if react.Type != pb.MessageReaction_THUMBS_UP {
		t.Errorf("Type: expected THUMBS_UP, got %v", react.Type)
	}
	if !react.Added {
		t.Error("expected Added=true")
	}
}

func TestHandleFeedbackButton_ThumbsDownForwarded(t *testing.T) {
	a, _ := newTestAdapter()
	srv := newFakeSlackServer(t, "")
	defer srv.Close()
	a.client = slacklib.New("xoxb-fake", slacklib.OptionAPIURL(srv.URL+"/"))

	fbHandler := &captureFeedbackHandler{}
	a.feedbackHandler = fbHandler.handle

	cb := &slacklib.InteractionCallback{
		User:    slacklib.User{ID: "U999"},
		Channel: slacklib.Channel{GroupConversation: slacklib.GroupConversation{Conversation: slacklib.Conversation{ID: "C123"}}},
	}
	cb.Message.Timestamp = "1700000000.000002"

	a.handleFeedbackButton(t.Context(), cb, &slacklib.BlockAction{ActionID: feedbackButtonsActionID, Value: "negative_feedback"})

	if fbHandler.count() != 1 {
		t.Fatalf("expected 1 forwarded feedback, got %d", fbHandler.count())
	}
	if got := fbHandler.last().GetReaction().Type; got != pb.MessageReaction_THUMBS_DOWN {
		t.Errorf("Type: expected THUMBS_DOWN, got %v", got)
	}
}

func TestHandleFeedbackButton_NilHandlerNoOps(t *testing.T) {
	a, _ := newTestAdapter()
	srv := newFakeSlackServer(t, "")
	defer srv.Close()
	a.client = slacklib.New("xoxb-fake", slacklib.OptionAPIURL(srv.URL+"/"))
	// no feedbackHandler set

	cb := &slacklib.InteractionCallback{
		User:    slacklib.User{ID: "U1"},
		Channel: slacklib.Channel{GroupConversation: slacklib.GroupConversation{Conversation: slacklib.Conversation{ID: "C1"}}},
	}

	// Must not panic even though feedbackHandler is nil.
	a.handleFeedbackButton(t.Context(), cb, &slacklib.BlockAction{ActionID: feedbackButtonsActionID, Value: "positive_feedback"})
}

func TestHandleFeedbackButton_ErrNoAgentStreamIsSwallowed(t *testing.T) {
	a, _ := newTestAdapter()
	srv := newFakeSlackServer(t, "")
	defer srv.Close()
	a.client = slacklib.New("xoxb-fake", slacklib.OptionAPIURL(srv.URL+"/"))

	a.feedbackHandler = func(_ context.Context, _ *pb.PlatformFeedback) error {
		return adapter.ErrNoAgentStream
	}

	cb := &slacklib.InteractionCallback{
		User:    slacklib.User{ID: "U1"},
		Channel: slacklib.Channel{GroupConversation: slacklib.GroupConversation{Conversation: slacklib.Conversation{ID: "C1"}}},
	}

	// ErrNoAgentStream is normal during dev; should not propagate (handler is non-fatal).
	a.handleFeedbackButton(t.Context(), cb, &slacklib.BlockAction{ActionID: feedbackButtonsActionID, Value: "positive_feedback"})
}

func TestHandleViewSubmission_TextForwarded(t *testing.T) {
	a, _ := newTestAdapter()
	srv := newFakeSlackServer(t, "")
	defer srv.Close()
	a.client = slacklib.New("xoxb-fake", slacklib.OptionAPIURL(srv.URL+"/"))

	fbHandler := &captureFeedbackHandler{}
	a.feedbackHandler = fbHandler.handle

	meta, _ := json.Marshal(map[string]string{
		"channel_id":      "C123",
		"message_ts":      "1700000000.000002",
		"thread_ts":       "1700000000.000001",
		"conversation_id": "C123-1700000000.000001",
	})

	cb := &slacklib.InteractionCallback{
		User: slacklib.User{ID: "U999", Name: "alice"},
		View: slacklib.View{
			CallbackID:      feedbackCommentCallbackID,
			PrivateMetadata: string(meta),
			State: &slacklib.ViewState{
				Values: map[string]map[string]slacklib.BlockAction{
					feedbackCommentInputBlockID: {
						feedbackCommentInputActionID: {Value: "  this rocked  "},
					},
				},
			},
		},
	}

	a.handleViewSubmission(t.Context(), nil, cb)

	if fbHandler.count() != 1 {
		t.Fatalf("expected 1 forwarded feedback, got %d", fbHandler.count())
	}
	fb := fbHandler.last()
	if fb.ConversationId != "C123-1700000000.000001" {
		t.Errorf("ConversationId: expected 'C123-1700000000.000001', got %q", fb.ConversationId)
	}
	if fb.ResponseId != "1700000000.000002" {
		t.Errorf("ResponseId: expected message_ts roundtrip, got %q", fb.ResponseId)
	}
	tf := fb.GetText()
	if tf == nil {
		t.Fatal("expected TextFeedback variant")
	}
	if tf.Text != "this rocked" {
		t.Errorf("Text: expected trimmed 'this rocked', got %q", tf.Text)
	}
	if tf.Prompt == "" {
		t.Error("expected Prompt to be set")
	}
}

func TestHandleViewSubmission_EmptySubmissionNotForwarded(t *testing.T) {
	a, _ := newTestAdapter()
	srv := newFakeSlackServer(t, "")
	defer srv.Close()
	a.client = slacklib.New("xoxb-fake", slacklib.OptionAPIURL(srv.URL+"/"))

	fbHandler := &captureFeedbackHandler{}
	a.feedbackHandler = fbHandler.handle

	meta, _ := json.Marshal(map[string]string{
		"channel_id":      "C123",
		"message_ts":      "1700000000.000002",
		"conversation_id": "C123-1700000000.000001",
	})

	cb := &slacklib.InteractionCallback{
		User: slacklib.User{ID: "U999"},
		View: slacklib.View{
			CallbackID:      feedbackCommentCallbackID,
			PrivateMetadata: string(meta),
			State: &slacklib.ViewState{
				Values: map[string]map[string]slacklib.BlockAction{
					feedbackCommentInputBlockID: {
						feedbackCommentInputActionID: {Value: "   "},
					},
				},
			},
		},
	}

	a.handleViewSubmission(t.Context(), nil, cb)

	if fbHandler.count() != 0 {
		t.Errorf("empty submission must not forward, got %d calls", fbHandler.count())
	}
}

func TestHandleViewSubmission_BadPrivateMetadataDoesNotForward(t *testing.T) {
	a, _ := newTestAdapter()
	srv := newFakeSlackServer(t, "")
	defer srv.Close()
	a.client = slacklib.New("xoxb-fake", slacklib.OptionAPIURL(srv.URL+"/"))

	fbHandler := &captureFeedbackHandler{}
	a.feedbackHandler = fbHandler.handle

	cb := &slacklib.InteractionCallback{
		View: slacklib.View{
			CallbackID:      feedbackCommentCallbackID,
			PrivateMetadata: "this is not json",
		},
	}

	a.handleViewSubmission(t.Context(), nil, cb)

	if fbHandler.count() != 0 {
		t.Errorf("bad metadata must not forward, got %d calls", fbHandler.count())
	}
}

func TestHandleViewSubmission_DifferentCallbackIDDropped(t *testing.T) {
	a, _ := newTestAdapter()
	fbHandler := &captureFeedbackHandler{}
	a.feedbackHandler = fbHandler.handle

	cb := &slacklib.InteractionCallback{
		View: slacklib.View{CallbackID: "some_other_modal"},
	}

	a.handleViewSubmission(t.Context(), nil, cb)

	if fbHandler.count() != 0 {
		t.Errorf("unrelated callback_id must not forward, got %d calls", fbHandler.count())
	}
}

func TestConversationIDFromCallback(t *testing.T) {
	tests := []struct {
		name     string
		callback *slacklib.InteractionCallback
		want     string
	}{
		{
			name: "uses container thread ts when present",
			callback: func() *slacklib.InteractionCallback {
				cb := &slacklib.InteractionCallback{
					Channel:   slacklib.Channel{GroupConversation: slacklib.GroupConversation{Conversation: slacklib.Conversation{ID: "C1"}}},
					Container: slacklib.Container{ThreadTs: "100.001"},
				}
				cb.Message.ThreadTimestamp = "should-not-use"
				cb.Message.Timestamp = "should-not-use-either"
				return cb
			}(),
			want: "C1-100.001",
		},
		{
			name: "falls back to message thread timestamp",
			callback: func() *slacklib.InteractionCallback {
				cb := &slacklib.InteractionCallback{
					Channel: slacklib.Channel{GroupConversation: slacklib.GroupConversation{Conversation: slacklib.Conversation{ID: "C2"}}},
				}
				cb.Message.ThreadTimestamp = "200.002"
				cb.Message.Timestamp = "should-not-use"
				return cb
			}(),
			want: "C2-200.002",
		},
		{
			name: "falls back to message timestamp",
			callback: func() *slacklib.InteractionCallback {
				cb := &slacklib.InteractionCallback{
					Channel: slacklib.Channel{GroupConversation: slacklib.GroupConversation{Conversation: slacklib.Conversation{ID: "C3"}}},
				}
				cb.Message.Timestamp = "300.003"
				return cb
			}(),
			want: "C3-300.003",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := conversationIDFromCallback(tt.callback); got != tt.want {
				t.Errorf("conversationIDFromCallback: expected %q, got %q", tt.want, got)
			}
		})
	}
}
