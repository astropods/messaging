package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/astropods/messaging/internal/store/files"
	pb "github.com/astropods/messaging/pkg/gen/astro/messaging/v1"
)

// filesLifecycleMux wires the full create→upload→read surface so a test can drive
// the reserved→ready lifecycle over HTTP.
func filesLifecycleMux(t *testing.T) (*http.ServeMux, files.FileStore) {
	t.Helper()
	fs, err := files.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	h := &Handlers{
		sessionManager: NewHeaderSessionManager("X-User-ID", "", ""),
		fileStore:      fs,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/files", h.HandleCreateFile)
	mux.HandleFunc("GET /api/files", h.HandleListFiles)
	mux.HandleFunc("GET /api/files/{key}", h.HandleGetFile)
	mux.HandleFunc("PUT /api/files/{key}/content", h.HandlePutFileContent)
	mux.HandleFunc("GET /api/files/{key}/content", h.HandleGetFileContent)
	return mux, fs
}

func fileReq(method, path, user, body string) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	r.Header.Set("X-User-ID", user)
	return r
}

func serve(mux *http.ServeMux, req *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

// listHasKey reports whether GET /api/files (as user) returns key.
func listHasKey(t *testing.T, mux *http.ServeMux, user, key string) bool {
	t.Helper()
	w := serve(mux, fileReq(http.MethodGet, "/api/files", user, ""))
	if w.Code != http.StatusOK {
		t.Fatalf("list: expected 200, got %d (%s)", w.Code, w.Body.String())
	}
	var resp listFilesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("list decode: %v", err)
	}
	for _, f := range resp.Files {
		if f.Key == key {
			return true
		}
	}
	return false
}

// A reserved upload (created but not yet PUT) must be invisible everywhere —
// not listed, not readable, not downloadable — until its bytes are committed,
// so an abandoned or in-flight upload never shows as a broken download chip.
func TestFileLifecycle_ReservedHiddenUntilReady(t *testing.T) {
	mux, _ := filesLifecycleMux(t)
	const user = "user-a"

	w := serve(mux, fileReq(http.MethodPost, "/api/files", user, `{"name":"a.txt","content_type":"text/plain","size":5}`))
	if w.Code != http.StatusOK {
		t.Fatalf("create: expected 200, got %d (%s)", w.Code, w.Body.String())
	}
	var created createFileResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("create decode: %v", err)
	}
	key := created.Key
	if key == "" {
		t.Fatal("create returned empty key")
	}

	// Reserved: hidden from list, metadata, and content.
	if listHasKey(t, mux, user, key) {
		t.Error("reserved file appeared in list")
	}
	if w := serve(mux, fileReq(http.MethodGet, "/api/files/"+key, user, "")); w.Code != http.StatusNotFound {
		t.Errorf("reserved metadata GET: expected 404, got %d", w.Code)
	}
	if w := serve(mux, fileReq(http.MethodGet, "/api/files/"+key+"/content", user, "")); w.Code != http.StatusNotFound {
		t.Errorf("reserved content GET: expected 404, got %d", w.Code)
	}

	// Commit the bytes.
	if w := serve(mux, fileReq(http.MethodPut, "/api/files/"+key+"/content", user, "hello")); w.Code != http.StatusOK {
		t.Fatalf("put: expected 200, got %d (%s)", w.Code, w.Body.String())
	}

	// Ready: now visible and downloadable.
	if !listHasKey(t, mux, user, key) {
		t.Error("ready file missing from list")
	}
	got := serve(mux, fileReq(http.MethodGet, "/api/files/"+key+"/content", user, ""))
	if got.Code != http.StatusOK || got.Body.String() != "hello" {
		t.Errorf("ready content GET: expected 200 'hello', got %d %q", got.Code, got.Body.String())
	}
}

// The declared size is a hard ceiling on the PUT: a client can't declare a tiny
// size to slip past the create-time capacity reserve and then stream a large
// body. Over-declared bytes are rejected (413) and the file stays reserved.
func TestFilePut_EnforcesDeclaredSizeCeiling(t *testing.T) {
	mux, _ := filesLifecycleMux(t)
	const user = "user-a"

	w := serve(mux, fileReq(http.MethodPost, "/api/files", user, `{"name":"a.txt","content_type":"text/plain","size":2}`))
	if w.Code != http.StatusOK {
		t.Fatalf("create: expected 200, got %d (%s)", w.Code, w.Body.String())
	}
	var created createFileResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("create decode: %v", err)
	}
	key := created.Key

	// Declared 2 bytes, send 11 → over the ceiling.
	if w := serve(mux, fileReq(http.MethodPut, "/api/files/"+key+"/content", user, "hello world")); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized put: expected 413, got %d (%s)", w.Code, w.Body.String())
	}
	// The failed upload never became ready.
	if listHasKey(t, mux, user, key) {
		t.Error("file became visible after a rejected oversized upload")
	}
}

// An agent that returns a storage key already owned by another user must NOT
// have that file transferred into the responding conversation: the attachment is
// omitted and the original owner is unchanged. An unowned (agent-written) file is
// still attributed to the conversation owner.
func TestResolveResponseAttachments_NoForeignOwnershipTransfer(t *testing.T) {
	dir := t.TempDir()
	fs, err := files.NewFSStore(dir)
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	ctx := context.Background()

	// user-a owns a ready managed file.
	if err := fs.WriteMeta(ctx, files.FileMeta{
		Key: "key-a", Name: "a.txt", ContentType: "text/plain", Size: 3,
		UploadedBy: "user-a", Status: files.StatusReady,
	}); err != nil {
		t.Fatalf("WriteMeta: %v", err)
	}
	if _, err := fs.WriteBlob(ctx, "key-a", strings.NewReader("abc")); err != nil {
		t.Fatalf("WriteBlob: %v", err)
	}

	fileAtt := func(name string) *pb.ResponseAttachment {
		return &pb.ResponseAttachment{
			AttachmentType: &pb.ResponseAttachment_File{File: &pb.FileAttachment{Filename: name}},
		}
	}

	// Agent replies in user-b's conversation echoing user-a's key → omitted.
	out := resolveResponseAttachments(ctx, fs, []*pb.ResponseAttachment{fileAtt("key-a")}, "user-b")
	if len(out) != 0 {
		t.Errorf("foreign-owned file surfaced into another conversation: %+v", out)
	}
	if m, _ := fs.ReadMeta(ctx, "key-a"); m.UploadedBy != "user-a" {
		t.Errorf("ownership transferred away from user-a: now %q", m.UploadedBy)
	}

	// An agent-written unowned file IS attributed to the conversation owner.
	if err := os.WriteFile(filepath.Join(dir, "agent-out.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write agent file: %v", err)
	}
	out = resolveResponseAttachments(ctx, fs, []*pb.ResponseAttachment{fileAtt("agent-out.txt")}, "user-b")
	if len(out) != 1 || out[0].Key != "agent-out.txt" {
		t.Fatalf("agent-written file not surfaced to owner: %+v", out)
	}
	if m, _ := fs.ReadMeta(ctx, "agent-out.txt"); m.UploadedBy != "user-b" {
		t.Errorf("agent file not attributed to conversation owner: got %q", m.UploadedBy)
	}
}
