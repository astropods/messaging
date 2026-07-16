package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/astropods/messaging/internal/store/files"
)

// filesTestMux wires the files routes against a header-based session manager so a
// request's X-User-ID header selects the acting user.
func filesTestMux(t *testing.T) (*http.ServeMux, files.FileStore) {
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
	mux.HandleFunc("GET /api/files", h.HandleListFiles)
	mux.HandleFunc("GET /api/files/{key}", h.HandleGetFile)
	mux.HandleFunc("DELETE /api/files/{key}", h.HandleDeleteFile)
	mux.HandleFunc("GET /api/files/{key}/content", h.HandleGetFileContent)
	return mux, fs
}

func do(mux *http.ServeMux, method, path, user string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("X-User-ID", user)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

// A file uploaded by user A must not be listable, readable, or deletable by user
// B (another authorized member of the same deployment). Before per-user scoping,
// list returned all files and content/get/delete had no owner check — this test
// fails on that code and passes with ownership enforced.
func TestFiles_PerUserIsolation(t *testing.T) {
	mux, fs := filesTestMux(t)
	ctx := context.Background()

	// Seed: user A owns "key-a".
	if err := fs.WriteMeta(ctx, files.FileMeta{
		Key: "key-a", Name: "a.txt", ContentType: "text/plain", Size: 3, UploadedBy: "user-a",
	}); err != nil {
		t.Fatalf("WriteMeta: %v", err)
	}
	if _, err := fs.WriteBlob(ctx, "key-a", strings.NewReader("abc")); err != nil {
		t.Fatalf("WriteBlob: %v", err)
	}

	// --- user B is denied everywhere ---
	if w := do(mux, http.MethodGet, "/api/files", "user-b"); w.Code == http.StatusOK {
		var resp listFilesResponse
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if len(resp.Files) != 0 {
			t.Errorf("user B list should be empty, got %d files", len(resp.Files))
		}
	} else {
		t.Errorf("user B list: expected 200, got %d", w.Code)
	}
	if w := do(mux, http.MethodGet, "/api/files/key-a/content", "user-b"); w.Code != http.StatusNotFound {
		t.Errorf("user B download of A's file: expected 404, got %d", w.Code)
	}
	if w := do(mux, http.MethodGet, "/api/files/key-a", "user-b"); w.Code != http.StatusNotFound {
		t.Errorf("user B get A's metadata: expected 404, got %d", w.Code)
	}
	if w := do(mux, http.MethodDelete, "/api/files/key-a", "user-b"); w.Code != http.StatusNotFound {
		t.Errorf("user B delete A's file: expected 404, got %d", w.Code)
	}

	// --- user A retains full access (and B's delete was a no-op) ---
	if w := do(mux, http.MethodGet, "/api/files/key-a/content", "user-a"); w.Code != http.StatusOK || w.Body.String() != "abc" {
		t.Errorf("user A download: expected 200 'abc', got %d %q", w.Code, w.Body.String())
	}
	w := do(mux, http.MethodGet, "/api/files", "user-a")
	var resp listFilesResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Files) != 1 || resp.Files[0].Key != "key-a" {
		t.Errorf("user A list should contain key-a, got %+v", resp.Files)
	}
}
