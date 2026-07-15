package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/astropods/messaging/internal/store/files"
	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
)

func sendHandlers(t *testing.T, fs files.FileStore, onForward func(*pb.Message)) *Handlers {
	t.Helper()
	return &Handlers{
		connManager:    NewConnectionManager(time.Second),
		sessionManager: NewHeaderSessionManager("X-User-ID", "", ""),
		fileStore:      fs,
		msgHandler: func(_ context.Context, msg *pb.Message) error {
			if onForward != nil {
				onForward(msg)
			}
			return nil
		},
	}
}

func sendReq(user, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/conversations/c1/messages", strings.NewReader(body))
	req.SetPathValue("id", "c1")
	req.Header.Set("X-User-ID", user)
	return req
}

// A user may only attach their OWN files: referencing another user's key is
// rejected (400) and never forwarded; the owner's own key is accepted and rides
// the message as a proto attachment carrying the storage key.
func TestSendMessage_AttachmentOwnership(t *testing.T) {
	fs, err := files.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	ctx := context.Background()
	if err := fs.WriteMeta(ctx, files.FileMeta{Key: "key-a", Name: "a.txt", ContentType: "text/plain", Size: 3, UploadedBy: "user-a"}); err != nil {
		t.Fatalf("WriteMeta: %v", err)
	}
	if _, err := fs.WriteBlob(ctx, "key-a", strings.NewReader("abc")); err != nil {
		t.Fatalf("WriteBlob: %v", err)
	}

	// user-b attaches user-a's key → rejected, nothing forwarded.
	var forwarded *pb.Message
	h := sendHandlers(t, fs, func(m *pb.Message) { forwarded = m })
	w := httptest.NewRecorder()
	h.HandleSendMessage(w, sendReq("user-b", `{"content":"hi","attachments":[{"key":"key-a"}]}`))
	if w.Code != http.StatusBadRequest {
		t.Errorf("foreign attachment: expected 400, got %d (%s)", w.Code, w.Body.String())
	}
	if forwarded != nil {
		t.Errorf("foreign attachment must not be forwarded to the agent")
	}

	// user-a attaches their own key → accepted + forwarded with the attachment.
	forwarded = nil
	w = httptest.NewRecorder()
	h.HandleSendMessage(w, sendReq("user-a", `{"content":"hi","attachments":[{"key":"key-a"}]}`))
	if w.Code != http.StatusOK {
		t.Fatalf("own attachment: expected 200, got %d (%s)", w.Code, w.Body.String())
	}
	if forwarded == nil || len(forwarded.Attachments) != 1 || forwarded.Attachments[0].StorageKey != "key-a" {
		t.Errorf("expected forwarded message to carry attachment key-a, got %+v", forwarded.GetAttachments())
	}
}

// More than maxAttachmentsPerMessage attachments is rejected before any
// resolution/forwarding, bounding fan-out.
func TestSendMessage_AttachmentCap(t *testing.T) {
	var forwarded *pb.Message
	h := sendHandlers(t, nil, func(m *pb.Message) { forwarded = m })

	parts := make([]string, maxAttachmentsPerMessage+1)
	for i := range parts {
		parts[i] = fmt.Sprintf(`{"key":"k%d"}`, i)
	}
	body := `{"content":"hi","attachments":[` + strings.Join(parts, ",") + `]}`

	w := httptest.NewRecorder()
	h.HandleSendMessage(w, sendReq("user-a", body))
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("over-cap send: expected 413, got %d (%s)", w.Code, w.Body.String())
	}
	if forwarded != nil {
		t.Errorf("over-cap send must not be forwarded")
	}
}
