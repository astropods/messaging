package slack

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/astropods/messaging/internal/store/files"
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

func TestHandleAppMention_FileSkippedWithoutFileStore(t *testing.T) {
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
	if want := "[slack_file_skipped] notes.pdf: " + skipNoStore; !strings.Contains(msg.Content, want) {
		t.Errorf("content %q does not contain %q", msg.Content, want)
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

// A :ticket: on an uncaptioned screenshot must reach the agent. Slack leaves
// `text` empty on such a message and renderBlocks drops image blocks, so the
// reacted content is entirely in the attachments.
func TestHandleReactionAdded_ImageOnlyMessageForwarded(t *testing.T) {
	a, handler := newTestAdapterWithReactions([]string{"ticket"})

	imgBytes := []byte("\x89PNG\r\n\x1a\nfake-image-bytes")
	mux := http.NewServeMux()
	mux.HandleFunc("/files/download", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(imgBytes)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/conversations.replies", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"messages": []map[string]any{{
				"ts":   r.FormValue("ts"),
				"text": "",
				"user": "U999",
				"files": []map[string]any{{
					"name":                 "screenshot.png",
					"mimetype":             "image/png",
					"size":                 len(imgBytes),
					"url_private_download": srv.URL + "/files/download",
				}},
			}},
		})
	})
	mux.HandleFunc("/", jsonOK)
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

	msg := handler.last()
	if msg == nil {
		t.Fatal("expected the reaction to be forwarded despite the message having no text")
	}
	if len(msg.Attachments) != 1 {
		t.Fatalf("expected the image to ride along, got %d attachments", len(msg.Attachments))
	}
	want := "data:image/png;base64," + base64.StdEncoding.EncodeToString(imgBytes)
	if msg.Attachments[0].Url != want {
		t.Errorf("url mismatch:\n got %q\nwant %q", msg.Attachments[0].Url, want)
	}
	if !strings.Contains(msg.Content, "[reaction :ticket:") {
		t.Errorf("expected the reaction preamble in content, got %q", msg.Content)
	}
}

// The empty-text guard still earns its place when there is genuinely nothing
// to act on: no text and no image means the agent would see a bare preamble.
func TestHandleReactionAdded_NoTextNoImageDropped(t *testing.T) {
	a, handler := newTestAdapterWithReactions([]string{"ticket"})
	srv := newFakeSlackServer(t, "")
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

	if handler.count() != 0 {
		t.Errorf("expected a contentless reacted message to be dropped, got %d messages", handler.count())
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

// Points the adapter's Slack clients at the server.
func fileTestServer(t *testing.T, a *SlackAdapter, body []byte) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/files/download", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	})
	mux.HandleFunc("/", jsonOK)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	a.client = slacklib.New("xoxb-fake", slacklib.OptionAPIURL(srv.URL+"/"))
	setFakeAIClient(a, srv)
	return srv.URL + "/files/download"
}

func newTestFileStore(t *testing.T, a *SlackAdapter) files.FileStore {
	t.Helper()
	s, err := files.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a.SetFileStore(s)
	return s
}

func assertStoredFile(t *testing.T, s files.FileStore, key string, want []byte, owner string) {
	t.Helper()
	meta, err := s.ReadMeta(t.Context(), key)
	if err != nil {
		t.Fatalf("read meta: %v", err)
	}
	if meta.UploadedBy != owner || !meta.Ready() {
		t.Errorf("meta: got owner %q ready %v, want owner %q ready", meta.UploadedBy, meta.Ready(), owner)
	}
	rc, err := s.OpenBlob(t.Context(), key)
	if err != nil {
		t.Fatalf("open blob: %v", err)
	}
	defer func() { _ = rc.Close() }()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read blob: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("blob: got %q, want %q", got, want)
	}
}

func assertNoStoredFiles(t *testing.T, s files.FileStore) {
	t.Helper()
	list, err := s.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Errorf("expected no stored files, got %d", len(list))
	}
}

func TestHandleAppMention_FileAttachmentStaged(t *testing.T) {
	a, handler := newTestAdapter()
	store := newTestFileStore(t, a)
	pdf := []byte("%PDF-1.7\nfake-pdf-bytes")
	url := fileTestServer(t, a, pdf)

	a.handleAppMention(t.Context(), &slackevents.AppMentionEvent{
		User:      "U123",
		Channel:   "C123",
		Text:      "<@U0BOT> update SFDC from this",
		TimeStamp: "1234567890.000001",
		Files: []slacklib.File{{
			Name:               "plan.pdf",
			Mimetype:           "application/pdf",
			Size:               len(pdf),
			URLPrivateDownload: url,
		}},
	}, "")

	msg := handler.last()
	if msg == nil || len(msg.Attachments) != 1 {
		t.Fatalf("expected one attachment, got %v", msg)
	}
	att := msg.Attachments[0]
	if att.Type != pb.Attachment_FILE || att.StorageKey == "" || att.Filename != "plan.pdf" ||
		att.MimeType != "application/pdf" || att.SizeBytes != int64(len(pdf)) {
		t.Fatalf("attachment: %+v", att)
	}
	assertStoredFile(t, store, att.StorageKey, pdf, "U123")
}

func TestHandleMessage_DMFileShareStaged(t *testing.T) {
	a, handler := newTestAdapter()
	store := newTestFileStore(t, a)
	csv := []byte("account,arr\nAcme,120000\n")
	url := fileTestServer(t, a, csv)

	a.handleMessage(t.Context(), &slackevents.MessageEvent{
		Channel:   "D123456",
		User:      "U123",
		SubType:   "file_share",
		TimeStamp: "1234567890.000001",
		Message: &slacklib.Msg{Files: []slacklib.File{{
			Name:               "deals.csv",
			Mimetype:           "text/csv",
			Size:               len(csv),
			URLPrivateDownload: url,
		}}},
	}, "", nil)

	msg := handler.last()
	if msg == nil || len(msg.Attachments) != 1 {
		t.Fatalf("expected one attachment, got %v", msg)
	}
	assertStoredFile(t, store, msg.Attachments[0].StorageKey, csv, "U123")
}

func TestHandleAppMention_FileNotStagedWhenDenied(t *testing.T) {
	a, handler := newTestAdapter()
	store := newTestFileStore(t, a)
	a.SetAuthorizer(&denyAuthorizer{})
	url := fileTestServer(t, a, []byte("%PDF-1.7"))

	a.handleAppMention(t.Context(), &slackevents.AppMentionEvent{
		User:      "U123",
		Channel:   "C123",
		Text:      "<@U0BOT> read this",
		TimeStamp: "1234567890.000001",
		Files:     []slacklib.File{{Name: "plan.pdf", Mimetype: "application/pdf", Size: 8, URLPrivateDownload: url}},
	}, "")

	if handler.count() != 0 {
		t.Fatalf("expected no dispatch on deny, got %d", handler.count())
	}
	assertNoStoredFiles(t, store)
}

func TestHandleAppMention_FileSignInPageSkipped(t *testing.T) {
	a, handler := newTestAdapter()
	store := newTestFileStore(t, a)
	page := []byte("<!DOCTYPE html><html><body>Sign in to Slack</body></html>")
	url := fileTestServer(t, a, page)

	a.handleAppMention(t.Context(), &slackevents.AppMentionEvent{
		User:      "U123",
		Channel:   "C123",
		Text:      "<@U0BOT> read this",
		TimeStamp: "1234567890.000001",
		Files:     []slacklib.File{{Name: "plan.pdf", Mimetype: "application/pdf", Size: len(page), URLPrivateDownload: url}},
	}, "")

	msg := handler.last()
	if msg == nil || len(msg.Attachments) != 0 {
		t.Fatalf("expected the message with no attachments, got %v", msg)
	}
	if want := "read this\n\n[slack_file_skipped] plan.pdf: " + skipRefused; !strings.HasSuffix(msg.Content, want) {
		t.Errorf("content %q does not end with %q", msg.Content, want)
	}
	assertNoStoredFiles(t, store)
}

func TestHandleMessage_ChannelFileShareDropped(t *testing.T) {
	a, handler := newTestAdapter()

	a.handleMessage(t.Context(), &slackevents.MessageEvent{
		Channel:         "C123456",
		User:            "U123",
		Text:            "fyi",
		SubType:         "file_share",
		ThreadTimeStamp: "1234567890.000001",
		TimeStamp:       "1234567890.000002",
	}, "", nil)

	if handler.count() != 0 {
		t.Fatalf("expected channel file_share to be dropped, got %d", handler.count())
	}
}

type slackUpload struct {
	filename string
	body     []byte
	channel  string
	threadTS string
}

// The fake Slack API implements the three-step file upload.
func uploadTestAdapter(t *testing.T) (*SlackAdapter, func() ([]slackUpload, []string)) {
	t.Helper()
	var (
		mu      sync.Mutex
		uploads []slackUpload
		posts   []string
		pending slackUpload
	)
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/conversations.replies", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "messages": []map[string]any{{"ts": r.Form.Get("ts")}}})
	})
	mux.HandleFunc("/files.getUploadURLExternal", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		pending = slackUpload{filename: r.Form.Get("filename")}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "upload_url": srv.URL + "/upload", "file_id": "F1"})
	})
	mux.HandleFunc("/upload", func(w http.ResponseWriter, r *http.Request) {
		f, _, err := r.FormFile("file")
		if err != nil {
			t.Errorf("upload form: %v", err)
			return
		}
		body, _ := io.ReadAll(f)
		mu.Lock()
		pending.body = body
		mu.Unlock()
	})
	mux.HandleFunc("/files.completeUploadExternal", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		pending.channel, pending.threadTS = r.Form.Get("channel_id"), r.Form.Get("thread_ts")
		uploads = append(uploads, pending)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "files": []map[string]any{{"id": "F1"}}})
	})
	mux.HandleFunc("/chat.postMessage", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		posts = append(posts, r.Form.Get("text"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "1234567890.000009", "channel": "C123"})
	})
	mux.HandleFunc("/", jsonOK)
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	a := New()
	a.client = slacklib.New("xoxb-fake", slacklib.OptionAPIURL(srv.URL+"/"))
	setFakeAIClient(a, srv)
	a.rateLimiter = NewRateLimiter(100, 10)
	return a, func() ([]slackUpload, []string) {
		mu.Lock()
		defer mu.Unlock()
		return append([]slackUpload(nil), uploads...), append([]string(nil), posts...)
	}
}

func sendReply(t *testing.T, a *SlackAdapter, text string, filenames ...string) {
	t.Helper()
	const convID = "C123-1234567890.000001"
	end := &pb.ContentChunk{Type: pb.ContentChunk_END}
	for _, name := range filenames {
		end.Attachments = append(end.Attachments, &pb.ResponseAttachment{
			AttachmentType: &pb.ResponseAttachment_File{File: &pb.FileAttachment{Filename: name}},
		})
	}
	for _, chunk := range []*pb.ContentChunk{
		{Type: pb.ContentChunk_START},
		{Type: pb.ContentChunk_DELTA, Content: text},
		end,
	} {
		if err := a.HandleAgentResponse(t.Context(), &pb.AgentResponse{
			ConversationId: convID,
			Payload:        &pb.AgentResponse_Content{Content: chunk},
		}); err != nil {
			t.Fatalf("HandleAgentResponse: %v", err)
		}
	}
}

func TestHandleAgentResponse_PostsAgentFileToThread(t *testing.T) {
	a, recorded := uploadTestAdapter(t)
	dir := t.TempDir()
	store, err := files.NewFSStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	a.SetFileStore(store)
	pdf := []byte("%PDF-1.7\naccount plan")
	if err := os.WriteFile(filepath.Join(dir, "account-plan.pdf"), pdf, 0o600); err != nil {
		t.Fatal(err)
	}

	sendReply(t, a, "Here is the account plan.", "account-plan.pdf")

	uploads, posts := recorded()
	if len(posts) != 1 {
		t.Errorf("expected the text reply to post once, got %q", posts)
	}
	if len(uploads) != 1 {
		t.Fatalf("expected one upload, got %d", len(uploads))
	}
	got := uploads[0]
	if got.filename != "account-plan.pdf" || !bytes.Equal(got.body, pdf) ||
		got.channel != "C123" || got.threadTS != "1234567890.000001" {
		t.Errorf("upload: %+v", got)
	}
}

func TestHandleAgentResponse_FileOnlyReplyPostsFile(t *testing.T) {
	a, recorded := uploadTestAdapter(t)
	dir := t.TempDir()
	store, err := files.NewFSStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	a.SetFileStore(store)
	if err := os.WriteFile(filepath.Join(dir, "deals.csv"), []byte("account,arr\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	sendReply(t, a, "", "deals.csv")

	uploads, posts := recorded()
	if len(posts) != 0 {
		t.Errorf("expected no text post for an empty reply, got %q", posts)
	}
	if len(uploads) != 1 {
		t.Fatalf("expected one upload, got %d", len(uploads))
	}
}

func TestHandleAgentResponse_UserOwnedFileNotPosted(t *testing.T) {
	a, recorded := uploadTestAdapter(t)
	store := newTestFileStore(t, a)
	if _, err := store.WriteBlob(t.Context(), "k1", strings.NewReader("private")); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteMeta(t.Context(), files.FileMeta{
		Key: "k1", Name: "private.pdf", Size: 7, UpdatedAt: time.Now(), UploadedBy: "U999", Status: files.StatusReady,
	}); err != nil {
		t.Fatal(err)
	}

	sendReply(t, a, "Here it is.", "k1")

	uploads, posts := recorded()
	if len(uploads) != 0 {
		t.Fatalf("expected a user-owned file not to be posted, got %d uploads", len(uploads))
	}
	if want := ":warning: k1 could not be sent: " + sendUserFile; len(posts) != 2 || posts[1] != want {
		t.Errorf("expected the reply and then %q, got %q", want, posts)
	}
}

func TestHandleAppMention_OversizedFilesNoted(t *testing.T) {
	a, handler := newTestAdapter()
	store := newTestFileStore(t, a)
	var downloads int
	mux := http.NewServeMux()
	mux.HandleFunc("/files/download", func(w http.ResponseWriter, _ *http.Request) { downloads++ })
	mux.HandleFunc("/", jsonOK)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	a.client = slacklib.New("xoxb-fake", slacklib.OptionAPIURL(srv.URL+"/"))
	setFakeAIClient(a, srv)
	url := srv.URL + "/files/download"

	a.handleAppMention(t.Context(), &slackevents.AppMentionEvent{
		User:      "U123",
		Channel:   "C123",
		Text:      "<@U0BOT> bulk update from these",
		TimeStamp: "1234567890.000001",
		Files: []slacklib.File{
			{Name: "export.csv", Mimetype: "text/csv", Size: int(files.DefaultMaxFileBytes) + 1, URLPrivateDownload: url},
		},
	}, "")

	msg := handler.last()
	if msg == nil || len(msg.Attachments) != 0 {
		t.Fatalf("expected the message with no attachments, got %v", msg)
	}
	if want := "[slack_file_skipped] export.csv: file is larger than 100 MiB"; !strings.Contains(msg.Content, want) {
		t.Errorf("content %q does not contain %q", msg.Content, want)
	}
	if downloads != 0 {
		t.Errorf("expected no downloads for oversized files, got %d", downloads)
	}
	assertNoStoredFiles(t, store)
}

func TestHandleMessage_DMImageInlined(t *testing.T) {
	a, handler := newTestAdapter()
	png := []byte("\x89PNG\r\n\x1a\nfake-image-bytes")
	url := fileTestServer(t, a, png)

	a.handleMessage(t.Context(), &slackevents.MessageEvent{
		Channel:   "D123456",
		User:      "U123",
		SubType:   "file_share",
		TimeStamp: "1234567890.000001",
		Message: &slacklib.Msg{Files: []slacklib.File{{
			Name:               "screenshot.png",
			Mimetype:           "image/png",
			Size:               len(png),
			URLPrivateDownload: url,
		}}},
	}, "", nil)

	msg := handler.last()
	if msg == nil || len(msg.Attachments) != 1 || msg.Attachments[0].Type != pb.Attachment_IMAGE {
		t.Fatalf("expected one IMAGE attachment, got %v", msg)
	}
	if msg.Content != "" {
		t.Errorf("expected no skip notes, got %q", msg.Content)
	}
}

func TestWithSkipNotes(t *testing.T) {
	skips := []fileSkip{{"a.pdf", skipRefused}, {"b.csv", skipNoLink}}
	if got, want := withSkipNotes("", skips), "[slack_file_skipped] a.pdf: "+skipRefused+"\n[slack_file_skipped] b.csv: "+skipNoLink; got != want {
		t.Errorf("empty content: got %q, want %q", got, want)
	}
	if got := withSkipNotes("hello", nil); got != "hello" {
		t.Errorf("no skips: got %q", got)
	}
}

func TestHandleAppMention_OversizedImageStagedAsFile(t *testing.T) {
	a, handler := newTestAdapter()
	store := newTestFileStore(t, a)
	png := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0}, maxInlineImageBytes)...)
	url := fileTestServer(t, a, png)

	a.handleAppMention(t.Context(), &slackevents.AppMentionEvent{
		User:      "U123",
		Channel:   "C123",
		Text:      "<@U0BOT> what is in this photo?",
		TimeStamp: "1234567890.000001",
		Files:     []slacklib.File{{Name: "photo.png", Mimetype: "image/png", Size: len(png), URLPrivateDownload: url}},
	}, "")

	msg := handler.last()
	if msg == nil || len(msg.Attachments) != 1 || msg.Attachments[0].Type != pb.Attachment_FILE {
		t.Fatalf("expected one FILE attachment, got %v", msg)
	}
	if strings.Contains(msg.Content, "slack_file_skipped") {
		t.Errorf("expected no skip note, got %q", msg.Content)
	}
	assertStoredFile(t, store, msg.Attachments[0].StorageKey, png, "U123")
}

func TestHandleAppMention_ConfiguredLimitStopsLongerDownload(t *testing.T) {
	a, handler := newTestAdapter()
	store := newTestFileStore(t, a)
	a.fileLimit = 16
	// Slack declares 8 bytes, but the download runs past the limit.
	url := fileTestServer(t, a, []byte("account,arr\nAcme,120000\nGlobex,45000\n"))

	a.handleAppMention(t.Context(), &slackevents.AppMentionEvent{
		User:      "U123",
		Channel:   "C123",
		Text:      "<@U0BOT> read this",
		TimeStamp: "1234567890.000001",
		Files:     []slacklib.File{{Name: "deals.csv", Mimetype: "text/csv", Size: 8, URLPrivateDownload: url}},
	}, "")

	msg := handler.last()
	if msg == nil || len(msg.Attachments) != 0 {
		t.Fatalf("expected the message with no attachments, got %v", msg)
	}
	if want := "[slack_file_skipped] deals.csv: file is larger than 0 MiB"; !strings.Contains(msg.Content, want) {
		t.Errorf("content %q does not contain %q", msg.Content, want)
	}
	assertNoStoredFiles(t, store)
}

func TestOversizedImageSkips(t *testing.T) {
	if got := oversizedImageSkips([]slacklib.File{
		{Name: "big.png", Mimetype: "image/png", Size: maxInlineImageBytes + 1},
		{Name: "small.png", Mimetype: "image/png", Size: 10},
		{Name: "big.pdf", Mimetype: "application/pdf", Size: maxInlineImageBytes + 1},
	}); len(got) != 1 || got[0].name != "big.png" || got[0].reason != skipImageTooLarge {
		t.Errorf("oversizedImageSkips = %v, want only big.png", got)
	}
}

func threadServer(t *testing.T, a *SlackAdapter, messages []map[string]any, body []byte) (url string, downloads func() int) {
	t.Helper()
	var mu sync.Mutex
	n := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/conversations.replies", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "messages": messages})
	})
	mux.HandleFunc("/files/download", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		n++
		mu.Unlock()
		_, _ = w.Write(body)
	})
	mux.HandleFunc("/", jsonOK)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	a.client = slacklib.New("xoxb-fake", slacklib.OptionAPIURL(srv.URL+"/"))
	setFakeAIClient(a, srv)
	return srv.URL + "/files/download", func() int { mu.Lock(); defer mu.Unlock(); return n }
}

func TestHandleAppMention_InThreadStagesEarlierFiles(t *testing.T) {
	a, handler := newTestAdapter()
	store := newTestFileStore(t, a)
	pdf := []byte("%PDF-1.7\nearlier deal doc")
	var url string
	file := func(id, name, mime string) map[string]any {
		return map[string]any{"id": id, "name": name, "mimetype": mime, "size": len(pdf), "url_private_download": "URL"}
	}
	messages := []map[string]any{
		{"ts": "11.1", "text": "here is the deal doc", "user": "U1", "files": []map[string]any{file("F1", "deal.pdf", "application/pdf")}},
		{"ts": "11.2", "text": "and a screenshot", "user": "U2", "files": []map[string]any{file("F2", "shot.png", "image/png")}},
		{"ts": "11.3", "text": "update SFDC from the doc", "user": "U3"},
	}
	url, _ = threadServer(t, a, messages, pdf)
	for _, m := range messages {
		if fs, ok := m["files"].([]map[string]any); ok {
			for _, f := range fs {
				f["url_private_download"] = url
			}
		}
	}

	a.handleAppMention(t.Context(), &slackevents.AppMentionEvent{
		User:            "U3",
		Channel:         "C1",
		Text:            "<@U0BOT> update SFDC from the doc",
		TimeStamp:       "11.3",
		ThreadTimeStamp: "11.1",
	}, "")

	msg := handler.last()
	if msg == nil {
		t.Fatal("expected a dispatched message")
	}
	names := map[string]string{}
	for _, att := range msg.Attachments {
		if att.Type != pb.Attachment_FILE {
			t.Errorf("earlier thread file %q should be staged as FILE, got %v", att.Filename, att.Type)
		}
		names[att.Filename] = att.StorageKey
	}
	if names["deal.pdf"] != "slack-F1" || names["shot.png"] != "slack-F2" {
		t.Fatalf("expected both earlier files keyed by Slack ID, got %v", names)
	}
	assertStoredFile(t, store, "slack-F1", pdf, "U3")
}

func TestStageFile_ReusesAFileAlreadyStaged(t *testing.T) {
	a, _ := newTestAdapter()
	store := newTestFileStore(t, a)
	pdf := []byte("%PDF-1.7\nsame file twice")
	url, downloads := threadServer(t, a, nil, pdf)
	f := slacklib.File{ID: "F9", Name: "plan.pdf", Mimetype: "application/pdf", Size: len(pdf), URLPrivateDownload: url}

	first, _ := a.stageFile(t.Context(), &f, "U1")
	second, _ := a.stageFile(t.Context(), &f, "U2")

	if first == nil || second == nil || first.StorageKey != "slack-F9" || second.StorageKey != "slack-F9" {
		t.Fatalf("expected both stagings to use key slack-F9, got %v and %v", first, second)
	}
	if got := downloads(); got != 1 {
		t.Errorf("expected one download for a file staged twice, got %d", got)
	}
	assertStoredFile(t, store, "slack-F9", pdf, "U1")
}

func TestThreadFiles_SkipsCurrentMessageDedupesAndCaps(t *testing.T) {
	msgs := []slacklib.Message{
		{Msg: slacklib.Msg{Timestamp: "1", Files: []slacklib.File{{ID: "A"}, {ID: "B"}}}},
		{Msg: slacklib.Msg{Timestamp: "2", Files: []slacklib.File{{ID: "A"}}}},
		{Msg: slacklib.Msg{Timestamp: "3", Files: []slacklib.File{{ID: "C"}}}},
	}
	var ids []string
	for _, f := range threadFiles(msgs, "3") {
		ids = append(ids, f.ID)
	}
	if strings.Join(ids, ",") != "A,B" {
		t.Errorf("threadFiles = %v, want [A B]: newest first, each once, current message excluded", ids)
	}

	var many []slacklib.Message
	for i := 0; i < threadFilesMax+5; i++ {
		many = append(many, slacklib.Message{Msg: slacklib.Msg{Timestamp: fmt.Sprint(i), Files: []slacklib.File{{ID: fmt.Sprint("F", i)}}}})
	}
	if got := len(threadFiles(many, "")); got != threadFilesMax {
		t.Errorf("threadFiles kept %d files, want the cap %d", got, threadFilesMax)
	}
}

func TestHandleAppMention_NoFileLimitStagesALargeFile(t *testing.T) {
	a, handler := newTestAdapter()
	store := newTestFileStore(t, a)
	a.SetFileLimit(files.NoFileLimit)
	body := []byte("account,arr\nAcme,120000\n")
	url := fileTestServer(t, a, body)

	a.handleAppMention(t.Context(), &slackevents.AppMentionEvent{
		User:      "U123",
		Channel:   "C123",
		Text:      "<@U0BOT> read this",
		TimeStamp: "1234567890.000001",
		// Slack declares a size over the default cap.
		Files: []slacklib.File{{ID: "F1", Name: "big.csv", Mimetype: "text/csv", Size: int(files.DefaultMaxFileBytes) + 1, URLPrivateDownload: url}},
	}, "")

	msg := handler.last()
	if msg == nil || len(msg.Attachments) != 1 {
		t.Fatalf("expected the file staged with no limit, got %v", msg)
	}
	assertStoredFile(t, store, "slack-F1", body, "U123")
}

func TestHandleAgentResponse_FileOverLimitNotPosted(t *testing.T) {
	a, recorded := uploadTestAdapter(t)
	dir := t.TempDir()
	store, err := files.NewFSStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	a.SetFileStore(store)
	a.SetFileLimit(8)
	if err := os.WriteFile(filepath.Join(dir, "report.csv"), []byte("more than eight bytes"), 0o600); err != nil {
		t.Fatal(err)
	}

	sendReply(t, a, "Here is the report.", "report.csv")

	uploads, posts := recorded()
	if len(uploads) != 0 {
		t.Fatalf("expected no upload for a file over the limit, got %d", len(uploads))
	}
	// The limit is 8 bytes, which prints as 0 MiB.
	if want := ":warning: report.csv could not be sent: file is larger than 0 MiB"; len(posts) != 2 || posts[1] != want {
		t.Errorf("expected the reply and then %q, got %q", want, posts)
	}
}

func TestHandleAgentResponse_FilesNotSentShareOneEscapedNotice(t *testing.T) {
	a, recorded := uploadTestAdapter(t)
	newTestFileStore(t, a)

	sendReply(t, a, "Here are both.", "<!channel>.csv", "missing.md")

	want := ":warning: &lt;!channel&gt;.csv could not be sent: " + sendNotFound +
		"\n:warning: missing.md could not be sent: " + sendNotFound
	if _, posts := recorded(); len(posts) != 2 || posts[1] != want {
		t.Errorf("expected the reply and then %q, got %q", want, posts)
	}
}
