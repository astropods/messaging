package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/astropods/messaging/internal/store/files"
)

func TestCreateFileErrorsUseStandardEnvelope(t *testing.T) {
	store, err := files.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	h := &Handlers{
		sessionManager: NewHeaderSessionManager("X-User-ID", "", ""),
		fileStore:      store,
	}

	tests := []struct {
		name   string
		body   string
		user   string
		status int
		code   string
	}{
		{name: "authentication", body: `{}`, status: http.StatusUnauthorized, code: "authentication_required"},
		{name: "malformed JSON", body: `{`, user: "user-a", status: http.StatusBadRequest, code: "invalid_file_request"},
		{name: "invalid name", body: `{"name":"bad/name","size":1}`, user: "user-a", status: http.StatusBadRequest, code: "invalid_file_name"},
		{
			name:   "too large",
			body:   fmt.Sprintf(`{"name":"large.bin","size":%d}`, filesMaxUploadBytes+1),
			user:   "user-a",
			status: http.StatusRequestEntityTooLarge,
			code:   "file_too_large",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/files", strings.NewReader(tt.body))
			if tt.user != "" {
				req.Header.Set("X-User-ID", tt.user)
			}
			w := httptest.NewRecorder()
			h.HandleCreateFile(w, req)

			if w.Code != tt.status {
				t.Fatalf("status = %d, want %d: %s", w.Code, tt.status, w.Body.String())
			}
			if got := w.Header().Get("Content-Type"); got != "application/json" {
				t.Fatalf("Content-Type = %q, want application/json", got)
			}
			var apiErr fileErrorResponse
			if err := json.Unmarshal(w.Body.Bytes(), &apiErr); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if apiErr.Error != tt.code || apiErr.Details == "" {
				t.Fatalf("error = %+v, want code %q and details", apiErr, tt.code)
			}
		})
	}
}

func TestCreateFileStorageUnavailableUsesStandardEnvelope(t *testing.T) {
	h := &Handlers{sessionManager: NewHeaderSessionManager("X-User-ID", "", "")}
	req := httptest.NewRequest(http.MethodPost, "/api/files", strings.NewReader(`{"name":"a.txt","size":1}`))
	req.Header.Set("X-User-ID", "user-a")
	w := httptest.NewRecorder()

	h.HandleCreateFile(w, req)

	var apiErr fileErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &apiErr); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if w.Code != http.StatusNotFound || apiErr.Error != "file_storage_unavailable" {
		t.Fatalf("expected 404 file_storage_unavailable, got %d %+v", w.Code, apiErr)
	}
}
