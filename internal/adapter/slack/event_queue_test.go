package slack

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	slacklib "github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
)

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestChannelQueue_RunsOneChannelInOrderOneAtATime(t *testing.T) {
	var q channelQueue
	var mu sync.Mutex
	var got []int
	running, overlapped := 0, false
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		q.run("C1", func() {
			defer wg.Done()
			mu.Lock()
			running++
			overlapped = overlapped || running > 1
			mu.Unlock()
			time.Sleep(time.Millisecond)
			mu.Lock()
			running--
			got = append(got, i)
			mu.Unlock()
		})
	}
	wg.Wait()

	if overlapped {
		t.Error("two events in one channel ran at the same time; a thread's events must reach the agent one by one")
	}
	for i, v := range got {
		if v != i {
			t.Fatalf("events ran in order %v; one channel's events must run in arrival order", got)
		}
	}
	waitUntil(t, "the idle channel to be released", func() bool {
		q.mu.Lock()
		defer q.mu.Unlock()
		return len(q.pending) == 0
	})
}

func TestChannelQueue_RunsChannelsInParallel(t *testing.T) {
	var q channelQueue
	release := make(chan struct{})
	done := make(chan struct{})
	q.run("C1", func() { <-release })
	q.run("C2", func() { close(done) })

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("an event in C2 waited for a blocked event in C1")
	}
	close(release)
}

func TestEventChannel(t *testing.T) {
	tests := map[string]any{
		"message":               &slackevents.MessageEvent{Channel: "C1"},
		"app_mention":           &slackevents.AppMentionEvent{Channel: "C1"},
		"reaction_added":        &slackevents.ReactionAddedEvent{Item: slackevents.Item{Channel: "C1"}},
		"reaction_removed":      &slackevents.ReactionRemovedEvent{Item: slackevents.Item{Channel: "C1"}},
		"assistant_thread":      &slackevents.AssistantThreadStartedEvent{AssistantThread: slackevents.AssistantThread{ChannelID: "C1"}},
		"agent_session_stopped": &slackevents.AgentSessionStoppedEvent{Channel: "C1"},
	}
	for name, data := range tests {
		if got := eventChannel(slackevents.EventsAPIInnerEvent{Data: data}); got != "C1" {
			t.Errorf("%s: channel = %q, want C1 so it queues behind that channel's earlier events", name, got)
		}
	}
	if got := eventChannel(slackevents.EventsAPIInnerEvent{Data: &slackevents.ChannelCreatedEvent{}}); got != "" {
		t.Errorf("an event without a channel = %q, want the shared empty key", got)
	}
}

func TestHandleSocketEvent_SlowDownloadDoesNotDelayAnotherChannel(t *testing.T) {
	a, handler := newTestAdapter()
	newTestFileStore(t, a)
	release := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/files/download", func(w http.ResponseWriter, _ *http.Request) {
		select {
		case <-release:
		case <-time.After(2 * time.Second):
		}
		_, _ = w.Write([]byte("a,b\n"))
	})
	mux.HandleFunc("/", jsonOK)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	a.client = slacklib.New("xoxb-fake", slacklib.OptionAPIURL(srv.URL+"/"))
	setFakeAIClient(a, srv)
	a.socketClient = socketmode.New(a.client)

	event := func(ev *slackevents.MessageEvent) socketmode.Event {
		return socketmode.Event{
			Type:    socketmode.EventTypeEventsAPI,
			Data:    slackevents.EventsAPIEvent{TeamID: "T1", InnerEvent: slackevents.EventsAPIInnerEvent{Type: "message", Data: ev}},
			Request: &socketmode.Request{EnvelopeID: ev.Channel},
		}
	}
	a.handleSocketEvent(t.Context(), event(&slackevents.MessageEvent{
		Channel: "D1", User: "U1", Text: "see file", SubType: "file_share", TimeStamp: "1.1",
		Message: &slacklib.Msg{Files: []slacklib.File{{
			ID: "F1", Name: "big.csv", Mimetype: "text/csv", Size: 4, URLPrivateDownload: srv.URL + "/files/download",
		}}},
	}))
	a.handleSocketEvent(t.Context(), event(&slackevents.MessageEvent{
		Channel: "D2", User: "U2", Text: "hello", TimeStamp: "2.1",
	}))

	waitUntil(t, "the other DM to reach the agent", func() bool { return handler.count() >= 1 })
	handler.mu.Lock()
	first := handler.messages[0].ConversationId
	handler.mu.Unlock()
	if first != "D2" {
		t.Fatalf("first message to reach the agent was %q; a download in D1 must not hold up D2", first)
	}
	close(release)
	waitUntil(t, "the DM with the file to reach the agent", func() bool { return handler.count() == 2 })
	if msg := handler.last(); msg.ConversationId != "D1" || len(msg.Attachments) != 1 {
		t.Errorf("the slow DM must still arrive with its file, got conversation %q with %d attachments",
			msg.ConversationId, len(msg.Attachments))
	}
}
