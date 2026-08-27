package slack

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"time"

	"github.com/astropods/messaging/internal/store"
	slacklib "github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
)

func directoryServer(t *testing.T, calls *atomic.Int32) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/users.info", func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"user":{"id":"U1","name":"ada","real_name":"Ada Lovelace","profile":{"display_name":"Ada"}}}`))
	})
	mux.HandleFunc("/conversations.info", func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"channel":{"id":"C1","name":"eng-support"}}`))
	})
	mux.HandleFunc("/", jsonOK)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// Slack ids are opaque, so an unresolved thread renders as U0… and C0… in the
// user's chat history.
func TestDirectory_ResolvesAndCaches(t *testing.T) {
	var calls atomic.Int32
	srv := directoryServer(t, &calls)
	d := newSlackDirectory(slacklib.New("xoxb-fake", slacklib.OptionAPIURL(srv.URL+"/")))

	if got := d.userName(t.Context(), "U1"); got != "Ada" {
		t.Errorf("userName = %q, want Ada", got)
	}
	if got := d.channelName(t.Context(), "C1"); got != "eng-support" {
		t.Errorf("channelName = %q, want eng-support", got)
	}
	before := calls.Load()
	d.userName(t.Context(), "U1")
	d.channelName(t.Context(), "C1")
	if calls.Load() != before {
		t.Errorf("expected cached lookups, got %d more calls", calls.Load()-before)
	}
}

// The lookups need users:read and channels:read. An app without them must fall
// back to the id and must not re-ask Slack on every single message.
func TestDirectory_CachesFailuresAndFallsBackToTheID(t *testing.T) {
	var calls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/users.info", func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":false,"error":"missing_scope"}`))
	})
	mux.HandleFunc("/", jsonOK)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	d := newSlackDirectory(slacklib.New("xoxb-fake", slacklib.OptionAPIURL(srv.URL+"/")))

	for i := 0; i < 3; i++ {
		if got := d.userName(t.Context(), "U1"); got != "" {
			t.Fatalf("userName = %q, want empty so the caller falls back", got)
		}
	}
	if calls.Load() != 1 {
		t.Errorf("expected the failure cached after one call, got %d", calls.Load())
	}
}

func TestDirectory_NilSafeAndIgnoresEmptyIDs(t *testing.T) {
	var d *slackDirectory
	if d.userName(t.Context(), "U1") != "" || d.channelName(t.Context(), "C1") != "" {
		t.Error("a nil directory must resolve to empty, not panic")
	}
	real := newSlackDirectory(nil)
	if real.userName(t.Context(), "") != "" || real.channelName(t.Context(), "") != "" {
		t.Error("empty ids must not be looked up")
	}
}

// dispatch is the single path every slack ingress takes, so naming the channel
// there is what stops a new ingress point shipping without it.
func TestDispatch_NamesTheChannel(t *testing.T) {
	var calls atomic.Int32
	srv := directoryServer(t, &calls)
	a, handler := newTestAdapter()
	a.client = slacklib.New("xoxb-fake", slacklib.OptionAPIURL(srv.URL+"/"))
	a.directory = newSlackDirectory(a.client)
	setFakeAIClient(a, srv)

	a.handleMessage(t.Context(), &slackevents.MessageEvent{
		Channel: "C1", User: "U1", Text: "hello", TimeStamp: "2.0001", ThreadTimeStamp: "1.0001",
	}, "T1", nil)

	msg := handler.last()
	if msg == nil {
		t.Fatal("expected a dispatched message")
	}
	if msg.PlatformContext.ChannelName != "eng-support" {
		t.Errorf("ChannelName = %q, want eng-support", msg.PlatformContext.ChannelName)
	}
}

// Slack's conversations.replies leaves username empty on ordinary messages, so
// without this every author in a copied thread is a raw U… id.
func TestHydrateThread_NamesTheAuthors(t *testing.T) {
	var calls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/conversations.replies", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"messages":[{"type":"message","user":"U1","text":"hello","ts":"1.0001"}]}`))
	})
	mux.HandleFunc("/users.info", func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"user":{"id":"U1","name":"ada","real_name":"Ada Lovelace","profile":{"display_name":"Ada"}}}`))
	})
	mux.HandleFunc("/", jsonOK)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	a, _ := newTestAdapter()
	a.client = slacklib.New("xoxb-fake", slacklib.OptionAPIURL(srv.URL+"/"))
	a.directory = newSlackDirectory(a.client)

	store := store.NewThreadHistoryStore(10, 50, time.Hour)
	if err := a.HydrateThread(t.Context(), "C1-1.0001", store); err != nil {
		t.Fatalf("HydrateThread: %v", err)
	}
	msgs := store.GetHistory("C1-1.0001", 10, false).Messages
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0].User.Username != "Ada" {
		t.Errorf("Username = %q, want Ada", msgs[0].User.Username)
	}
}
