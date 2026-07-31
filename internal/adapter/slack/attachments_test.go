package slack

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
	slacklib "github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
)

func jsonOK(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// setFakeAIClient points the adapter's AI client at the test server so the
// SetThreadStatus call in handleAppMention hits the fake instead of panicking
// on a nil client.
func setFakeAIClient(a *SlackAdapter, srv *httptest.Server) {
	a.aiClient = &SlackAIClient{
		botToken:   "xoxb-fake",
		httpClient: srv.Client(),
		baseURL:    srv.URL,
	}
}

func TestHandleAppMention_ImageAttachmentInlined(t *testing.T) {
	a, handler := newTestAdapter()

	imgBytes := []byte("\x89PNG\r\n\x1a\nfake-image-bytes")
	mux := http.NewServeMux()
	mux.HandleFunc("/files/download", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(imgBytes)
	})
	mux.HandleFunc("/", jsonOK)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	a.client = slacklib.New("xoxb-fake", slacklib.OptionAPIURL(srv.URL+"/"))
	setFakeAIClient(a, srv)

	ev := &slackevents.AppMentionEvent{
		User:      "U123",
		Channel:   "C123",
		Text:      "<@U0BOT> look at this",
		TimeStamp: "1234567890.000001",
		Files: []slacklib.File{{
			Name:               "screenshot.png",
			Mimetype:           "image/png",
			Filetype:           "png",
			Size:               len(imgBytes),
			URLPrivateDownload: srv.URL + "/files/download",
			OriginalW:          100,
			OriginalH:          50,
		}},
	}

	a.handleAppMention(t.Context(), ev, "")

	msg := handler.last()
	if msg == nil {
		t.Fatal("expected a dispatched message")
	}
	if len(msg.Attachments) != 1 {
		t.Fatalf("expected 1 attachment, got %d", len(msg.Attachments))
	}
	att := msg.Attachments[0]
	if att.Type != pb.Attachment_IMAGE {
		t.Errorf("type: got %v, want IMAGE", att.Type)
	}
	want := "data:image/png;base64," + base64.StdEncoding.EncodeToString(imgBytes)
	if att.Url != want {
		t.Errorf("url mismatch:\n got %q\nwant %q", att.Url, want)
	}
	if att.MimeType != "image/png" {
		t.Errorf("mime: got %q", att.MimeType)
	}
	if att.Width != 100 || att.Height != 50 {
		t.Errorf("dims: got %dx%d, want 100x50", att.Width, att.Height)
	}
}

func TestHandleAppMention_NonImageFileSkipped(t *testing.T) {
	a, handler := newTestAdapter()
	mux := http.NewServeMux()
	mux.HandleFunc("/", jsonOK)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	a.client = slacklib.New("xoxb-fake", slacklib.OptionAPIURL(srv.URL+"/"))
	setFakeAIClient(a, srv)

	ev := &slackevents.AppMentionEvent{
		User:      "U123",
		Channel:   "C123",
		Text:      "<@U0BOT> here is a doc",
		TimeStamp: "1234567890.000001",
		Files: []slacklib.File{{
			Name:               "notes.pdf",
			Mimetype:           "application/pdf",
			URLPrivateDownload: srv.URL + "/files/download",
		}},
	}

	a.handleAppMention(t.Context(), ev, "")

	msg := handler.last()
	if msg == nil {
		t.Fatal("expected a dispatched message")
	}
	if len(msg.Attachments) != 0 {
		t.Fatalf("expected non-image file to be skipped, got %d attachments", len(msg.Attachments))
	}
}

func TestHandleAppMention_InThreadPrependsThreadSummary(t *testing.T) {
	a, handler := newTestAdapter()
	mux := http.NewServeMux()
	mux.HandleFunc("/conversations.replies", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"messages": []map[string]any{
				{"ts": "11.1", "text": "first message", "user": "U1"},
				{"ts": "11.2", "text": "second message", "user": "U2"},
				{"ts": "11.3", "text": "make a ticket", "user": "U3"},
			},
		})
	})
	mux.HandleFunc("/", jsonOK)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	a.client = slacklib.New("xoxb-fake", slacklib.OptionAPIURL(srv.URL+"/"))
	setFakeAIClient(a, srv)

	ev := &slackevents.AppMentionEvent{
		User:            "U3",
		Channel:         "C1",
		Text:            "<@U0BOT> make a ticket",
		TimeStamp:       "11.3",
		ThreadTimeStamp: "11.1",
	}

	a.handleAppMention(t.Context(), ev, "")

	msg := handler.last()
	if msg == nil {
		t.Fatal("expected a dispatched message")
	}
	if !strings.HasPrefix(msg.Content, "[slack_thread_summary]\n") {
		t.Fatalf("expected [slack_thread_summary] prefix, got: %q", msg.Content)
	}
	for _, want := range []string{"first message", "second message"} {
		if !strings.Contains(msg.Content, want) {
			t.Errorf("thread summary missing %q; content=%q", want, msg.Content)
		}
	}
}

func TestHandleAppMention_TopLevelNoThreadSummary(t *testing.T) {
	a, handler := newTestAdapter()
	mux := http.NewServeMux()
	mux.HandleFunc("/", jsonOK)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	a.client = slacklib.New("xoxb-fake", slacklib.OptionAPIURL(srv.URL+"/"))
	setFakeAIClient(a, srv)

	ev := &slackevents.AppMentionEvent{
		User:      "U3",
		Channel:   "C1",
		Text:      "<@U0BOT> hello",
		TimeStamp: "11.3",
		// ThreadTimeStamp empty: a top-level mention has no thread to summarize.
	}

	a.handleAppMention(t.Context(), ev, "")

	msg := handler.last()
	if msg == nil {
		t.Fatal("expected a dispatched message")
	}
	if strings.Contains(msg.Content, "[slack_thread_summary]") {
		t.Errorf("top-level mention should carry no thread summary; got: %q", msg.Content)
	}
}
