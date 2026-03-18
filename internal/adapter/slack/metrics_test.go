package slack

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/astropods/messaging/internal/metrics"
	"github.com/prometheus/client_golang/prometheus/testutil"
	slacklib "github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
)

// newFakeSlackClient creates a *slack.Client backed by a test HTTP server that
// returns a canned conversations.replies response containing one message at ts.
func newFakeSlackClient(ts string) (*slacklib.Client, func()) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"messages": []map[string]any{
				{"type": "message", "text": "original text", "ts": ts},
			},
		})
	}))
	client := slacklib.New("xoxb-test",
		slacklib.OptionAPIURL(server.URL+"/"),
		slacklib.OptionHTTPClient(server.Client()),
	)
	return client, server.Close
}

// --- MessagesDropped: bot_filtered ---

func TestMetrics_BotFiltered(t *testing.T) {
	a, _ := newTestAdapter()

	before := testutil.ToFloat64(metrics.MessagesDropped.WithLabelValues("slack", "bot_filtered"))

	a.handleMessage(t.Context(), &slackevents.MessageEvent{
		Channel:   "D123456",
		User:      "U123",
		BotID:     "B999",
		Text:      "bot says hi",
		TimeStamp: "1111111111.000001",
	})

	if got := testutil.ToFloat64(metrics.MessagesDropped.WithLabelValues("slack", "bot_filtered")) - before; got != 1 {
		t.Errorf("MessagesDropped{bot_filtered}: expected +1, got +%v", got)
	}
}

// --- MessagesDropped: allowlist (message) ---

func TestMetrics_AllowlistDropped_Message(t *testing.T) {
	slackClient, slackCleanup := newFakeSlackClient("0000000000.000001")
	defer slackCleanup()

	a, _ := newTestAdapter()
	a.config.AllowedChannelIDs = []string{"C_ALLOWED"}
	a.client = slackClient

	before := testutil.ToFloat64(metrics.MessagesDropped.WithLabelValues("slack", "allowlist"))

	a.handleMessage(t.Context(), &slackevents.MessageEvent{
		Channel:         "C_BLOCKED",
		User:            "U123",
		Text:            "not allowed",
		TimeStamp:       "2222222222.000001",
		ThreadTimeStamp: "2222222222.000000",
	})

	if got := testutil.ToFloat64(metrics.MessagesDropped.WithLabelValues("slack", "allowlist")) - before; got != 1 {
		t.Errorf("MessagesDropped{allowlist} (message): expected +1, got +%v", got)
	}
}

// --- MessagesDropped: allowlist (mention) ---

func TestMetrics_AllowlistDropped_Mention(t *testing.T) {
	slackClient, slackCleanup := newFakeSlackClient("0000000000.000001")
	defer slackCleanup()

	a, _ := newTestAdapter()
	a.config.AllowedChannelIDs = []string{"C_ALLOWED"}
	a.client = slackClient

	before := testutil.ToFloat64(metrics.MessagesDropped.WithLabelValues("slack", "allowlist"))

	a.handleAppMention(t.Context(), &slackevents.AppMentionEvent{
		Channel:   "C_BLOCKED",
		User:      "U123",
		Text:      "<@BOT> hello",
		TimeStamp: "3333333333.000001",
	})

	if got := testutil.ToFloat64(metrics.MessagesDropped.WithLabelValues("slack", "allowlist")) - before; got != 1 {
		t.Errorf("MessagesDropped{allowlist} (mention): expected +1, got +%v", got)
	}
}

// --- SlackEvents: dm ---

func TestMetrics_SlackEvent_DM(t *testing.T) {
	a, _ := newTestAdapter()

	before := testutil.ToFloat64(metrics.SlackEvents.WithLabelValues("dm"))

	a.handleMessage(t.Context(), &slackevents.MessageEvent{
		Channel:   "D123456",
		User:      "U123",
		Text:      "direct message",
		TimeStamp: "4444444444.000001",
	})

	if got := testutil.ToFloat64(metrics.SlackEvents.WithLabelValues("dm")) - before; got != 1 {
		t.Errorf("SlackEvents{dm}: expected +1, got +%v", got)
	}
}

// --- SlackEvents: thread_reply ---

func TestMetrics_SlackEvent_ThreadReply(t *testing.T) {
	a, _ := newTestAdapter()

	before := testutil.ToFloat64(metrics.SlackEvents.WithLabelValues("thread_reply"))

	a.handleMessage(t.Context(), &slackevents.MessageEvent{
		Channel:         "C123456",
		User:            "U123",
		Text:            "thread reply",
		TimeStamp:       "5555555555.000001",
		ThreadTimeStamp: "5555555555.000000",
	})

	if got := testutil.ToFloat64(metrics.SlackEvents.WithLabelValues("thread_reply")) - before; got != 1 {
		t.Errorf("SlackEvents{thread_reply}: expected +1, got +%v", got)
	}
}

// --- SlackEvents: mention ---

func TestMetrics_SlackEvent_Mention(t *testing.T) {
	aiClient, aiCleanup := newTestAIClient(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	defer aiCleanup()

	a, _ := newTestAdapter()
	a.aiClient = aiClient

	before := testutil.ToFloat64(metrics.SlackEvents.WithLabelValues("mention"))

	a.handleAppMention(t.Context(), &slackevents.AppMentionEvent{
		Channel:   "C123456",
		User:      "U123",
		Text:      "<@BOT> hello",
		TimeStamp: "6666666666.000001",
	})

	if got := testutil.ToFloat64(metrics.SlackEvents.WithLabelValues("mention")) - before; got != 1 {
		t.Errorf("SlackEvents{mention}: expected +1, got +%v", got)
	}
}

// --- SlackEvents: reaction ---

func TestMetrics_SlackEvent_Reaction(t *testing.T) {
	const itemTS = "7777777777.000001"

	slackClient, slackCleanup := newFakeSlackClient(itemTS)
	defer slackCleanup()

	a, _ := newTestAdapterWithReactions([]string{"thumbsup"})
	a.client = slackClient

	before := testutil.ToFloat64(metrics.SlackEvents.WithLabelValues("reaction"))

	a.handleReactionAdded(t.Context(), &slackevents.ReactionAddedEvent{
		Reaction: "thumbsup",
		User:     "U123",
		Item: slackevents.Item{
			Channel:   "C123456",
			Timestamp: itemTS,
		},
	})

	if got := testutil.ToFloat64(metrics.SlackEvents.WithLabelValues("reaction")) - before; got != 1 {
		t.Errorf("SlackEvents{reaction}: expected +1, got +%v", got)
	}
}
