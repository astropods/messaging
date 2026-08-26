package web

import (
	"context"
	"encoding/base64"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/astropods/messaging/internal/store/files"
	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
)

// onePixelPNG is a minimal valid PNG; http.DetectContentType sniffs it as image/png.
const onePixelPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

func storeFile(t *testing.T, fs files.FileStore, key, name, contentType string, body []byte) {
	t.Helper()
	ctx := context.Background()
	if err := fs.WriteMeta(ctx, files.FileMeta{
		Key: key, Name: name, ContentType: contentType,
		Size: int64(len(body)), UploadedBy: "user-a",
	}); err != nil {
		t.Fatalf("WriteMeta(%s): %v", key, err)
	}
	if _, err := fs.WriteBlob(ctx, key, strings.NewReader(string(body))); err != nil {
		t.Fatalf("WriteBlob(%s): %v", key, err)
	}
}

func sendWithAttachment(t *testing.T, fs files.FileStore, keys ...string) []*pb.Attachment {
	t.Helper()
	refs := make([]string, len(keys))
	for i, k := range keys {
		refs[i] = `{"key":"` + k + `"}`
	}
	var forwarded *pb.Message
	h := sendHandlers(t, fs, func(m *pb.Message) { forwarded = m })
	w := httptest.NewRecorder()
	h.HandleSendMessage(w, sendReq("user-a", `{"content":"what is this?","attachments":[`+strings.Join(refs, ",")+`]}`))
	if w.Code != 200 {
		t.Fatalf("send: expected 200, got %d (%s)", w.Code, w.Body.String())
	}
	if forwarded == nil {
		t.Fatal("expected the message to be forwarded")
	}
	return forwarded.Attachments
}

func onlyOfType(atts []*pb.Attachment, typ pb.Attachment_Type) []*pb.Attachment {
	var out []*pb.Attachment
	for _, a := range atts {
		if a.Type == typ {
			out = append(out, a)
		}
	}
	return out
}

// adapter-core drops anything that is not type IMAGE with a non-empty url, so
// without the inline copy the model never sees the image.
func TestSendMessage_ImageForwardedInline(t *testing.T) {
	raw, err := base64.StdEncoding.DecodeString(onePixelPNG)
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	fs, ferr := files.NewFSStore(t.TempDir())
	if ferr != nil {
		t.Fatalf("NewFSStore: %v", ferr)
	}
	storeFile(t, fs, "img-1", "shot.png", "image/png", raw)

	atts := sendWithAttachment(t, fs, "img-1")

	fileAtts := onlyOfType(atts, pb.Attachment_FILE)
	if len(fileAtts) != 1 || fileAtts[0].StorageKey != "img-1" {
		t.Errorf("expected the FILE attachment to survive, got %+v", fileAtts)
	}

	imgAtts := onlyOfType(atts, pb.Attachment_IMAGE)
	if len(imgAtts) != 1 {
		t.Fatalf("expected one inline IMAGE attachment, got %d (%+v)", len(imgAtts), atts)
	}
	img := imgAtts[0]
	if got, want := img.Url, "data:image/png;base64,"+onePixelPNG; got != want {
		t.Errorf("data URI = %q, want %q", got, want)
	}
	if img.MimeType != "image/png" {
		t.Errorf("mime = %q, want image/png", img.MimeType)
	}
	if img.SizeBytes != int64(len(raw)) {
		t.Errorf("size = %d, want %d", img.SizeBytes, len(raw))
	}
	if img.Filename != "shot.png" {
		t.Errorf("filename = %q, want shot.png", img.Filename)
	}
}

func TestSendMessage_NonImageNotInlined(t *testing.T) {
	fs, err := files.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	storeFile(t, fs, "doc-1", "notes.txt", "text/plain", []byte("hello"))

	atts := sendWithAttachment(t, fs, "doc-1")
	if len(atts) != 1 || atts[0].Type != pb.Attachment_FILE {
		t.Errorf("expected a single FILE attachment, got %+v", atts)
	}
}

// The model rejects a data URI whose label disagrees with its bytes.
func TestSendMessage_MislabelledImageNotInlined(t *testing.T) {
	fs, err := files.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	storeFile(t, fs, "fake-1", "fake.png", "image/png", []byte("<html>not an image</html>"))

	atts := sendWithAttachment(t, fs, "fake-1")
	if len(onlyOfType(atts, pb.Attachment_IMAGE)) != 0 {
		t.Errorf("mislabelled file must not be inlined, got %+v", atts)
	}
	if len(onlyOfType(atts, pb.Attachment_FILE)) != 1 {
		t.Errorf("expected the FILE attachment to survive, got %+v", atts)
	}
}

// Without a message-wide budget these two would overrun the gRPC frame.
func TestSendMessage_InlineBudgetSpansMessage(t *testing.T) {
	fs, err := files.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	raw, derr := base64.StdEncoding.DecodeString(onePixelPNG)
	if derr != nil {
		t.Fatalf("decode fixture: %v", derr)
	}
	// Two images that each fit alone but cannot both fit the budget.
	padded := append(raw, make([]byte, (maxInlineImageBytes/2)+1)...)
	storeFile(t, fs, "img-a", "a.png", "image/png", padded)
	storeFile(t, fs, "img-b", "b.png", "image/png", padded)

	atts := sendWithAttachment(t, fs, "img-a", "img-b")
	if got := len(onlyOfType(atts, pb.Attachment_IMAGE)); got != 1 {
		t.Errorf("inlined %d images, want 1 (budget must stop the second)", got)
	}
	if got := len(onlyOfType(atts, pb.Attachment_FILE)); got != 2 {
		t.Errorf("got %d FILE attachments, want 2 (both files must survive)", got)
	}

	var inlined int64
	for _, a := range onlyOfType(atts, pb.Attachment_IMAGE) {
		inlined += a.SizeBytes
	}
	if inlined > maxInlineImageBytes {
		t.Errorf("inlined %d raw bytes, over the %d budget", inlined, maxInlineImageBytes)
	}
}

func TestSendMessage_OversizedImageNotInlined(t *testing.T) {
	fs, err := files.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	raw, derr := base64.StdEncoding.DecodeString(onePixelPNG)
	if derr != nil {
		t.Fatalf("decode fixture: %v", derr)
	}
	big := append(raw, make([]byte, maxInlineImageBytes+1)...)
	storeFile(t, fs, "big-1", "big.png", "image/png", big)

	atts := sendWithAttachment(t, fs, "big-1")
	if len(onlyOfType(atts, pb.Attachment_IMAGE)) != 0 {
		t.Errorf("oversized image must not be inlined, got %d attachments", len(atts))
	}
	if len(onlyOfType(atts, pb.Attachment_FILE)) != 1 {
		t.Errorf("expected the FILE attachment to survive, got %+v", atts)
	}
}
